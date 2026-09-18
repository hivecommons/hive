package mention

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingWebhookPoller struct {
	mu    sync.Mutex
	repos []string
	ch    chan string
}

func (p *recordingWebhookPoller) PollRepo(ctx context.Context, repo string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos = append(p.repos, repo)
	if p.ch != nil {
		select {
		case p.ch <- repo:
		default:
		}
	}
}

func (p *recordingWebhookPoller) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.repos)
}

func (p *recordingWebhookPoller) first() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.repos) == 0 {
		return ""
	}
	return p.repos[0]
}

func signGitHubWebhook(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postMentionWebhook(w http.Handler, event, body, sig string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/github/mentions/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	w.ServeHTTP(rec, req)
	return rec
}

func TestWebhookReceiverVerifiesSignatureAndPollsRepo(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/repo"}}`
	poller := &recordingWebhookPoller{ch: make(chan string, 1)}
	w := NewWebhookReceiver(func() string { return "secret" }, poller, time.Millisecond, nil)
	rec := postMentionWebhook(w, "issue_comment", body, signGitHubWebhook(body, "secret"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case got := <-poller.ch:
		if got != "org/repo" {
			t.Fatalf("repo = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not trigger repo poll")
	}
	if got := poller.first(); got != "org/repo" {
		t.Fatalf("repo = %q", got)
	}
}

func TestWebhookReceiverFailClosedNoOracle(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/repo"}}`
	for _, tc := range []struct {
		name   string
		secret string
		sig    string
	}{
		{"missing secret", "", signGitHubWebhook(body, "secret")},
		{"missing signature", "secret", ""},
		{"wrong signature", "secret", signGitHubWebhook(body, "other")},
		{"bad prefix", "secret", strings.TrimPrefix(signGitHubWebhook(body, "secret"), "sha256=")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			poller := &recordingWebhookPoller{}
			w := NewWebhookReceiver(func() string { return tc.secret }, poller, time.Millisecond, nil)
			rec := postMentionWebhook(w, "issue_comment", body, tc.sig)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rec.Code)
			}
			if poller.count() != 0 {
				t.Fatalf("unsigned/mismatched webhook triggered %d polls", poller.count())
			}
		})
	}
}

func TestWebhookReceiverFiltersEventsAndCoalesces(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/repo"}}`
	issueOpened := `{"action":"opened","repository":{"full_name":"org/repo"}}`
	edited := `{"action":"edited","repository":{"full_name":"org/repo"}}`
	poller := &recordingWebhookPoller{ch: make(chan string, 2)}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	w := NewWebhookReceiver(func() string { return "secret" }, poller, time.Minute, nil)
	w.SetClock(func() time.Time { return now })

	for _, probe := range []struct {
		event string
		body  string
	}{
		{"issue_comment", edited},
		{"pull_request", body},
		{"issues", body},
		{"issue_comment", `{`},
		{"issue_comment", `{"action":"created","repository":{}}`},
	} {
		postMentionWebhook(w, probe.event, probe.body, signGitHubWebhook(probe.body, "secret"))
	}
	if poller.count() != 0 {
		t.Fatalf("filtered events triggered %d polls", poller.count())
	}

	postMentionWebhook(w, "pull_request_review_comment", body, signGitHubWebhook(body, "secret"))
	postMentionWebhook(w, "issues", issueOpened, signGitHubWebhook(issueOpened, "secret"))
	select {
	case <-poller.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not trigger first coalesced poll")
	}
	select {
	case got := <-poller.ch:
		t.Fatalf("coalesced webhook triggered extra poll for %q", got)
	default:
	}
	if poller.count() != 1 {
		t.Fatalf("coalesced count = %d, want 1", poller.count())
	}
	now = now.Add(time.Minute)
	postMentionWebhook(w, "issues", issueOpened, signGitHubWebhook(issueOpened, "secret"))
	select {
	case <-poller.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not trigger post-gap poll")
	}
	if poller.count() != 2 {
		t.Fatalf("post-gap count = %d, want 2", poller.count())
	}
}

func TestWebhookReceiverRejectsUnconfiguredRepoBeforeAdmit(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/other"}}`
	poller := &recordingWebhookPoller{}
	w := NewWebhookReceiver(func() string { return "secret" }, poller, time.Minute, nil)
	w.SetReposFunc(func() []string { return []string{"Org/Repo"} })
	rec := postMentionWebhook(w, "issue_comment", body, signGitHubWebhook(body, "secret"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if poller.count() != 0 {
		t.Fatalf("unconfigured repo triggered %d polls", poller.count())
	}
	if len(w.lastPoll) != 0 {
		t.Fatalf("unconfigured repo grew lastPoll: %+v", w.lastPoll)
	}
}

func TestWebhookReceiverConfiguredRepoCaseVariantUsesConfiguredKey(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/repo"}}`
	poller := &recordingWebhookPoller{ch: make(chan string, 2)}
	now := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)
	w := NewWebhookReceiver(func() string { return "secret" }, poller, time.Minute, nil)
	w.SetClock(func() time.Time { return now })
	w.SetReposFunc(func() []string { return []string{"Org/Repo"} })
	postMentionWebhook(w, "issue_comment", body, signGitHubWebhook(body, "secret"))
	postMentionWebhook(w, "issue_comment", body, signGitHubWebhook(body, "secret"))
	select {
	case got := <-poller.ch:
		if got != "Org/Repo" {
			t.Fatalf("poll repo = %q, want configured casing", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook did not trigger repo poll")
	}
	select {
	case got := <-poller.ch:
		t.Fatalf("case-variant repo did not coalesce; extra poll for %q", got)
	default:
	}
	if poller.count() != 1 {
		t.Fatalf("case-variant repo did not coalesce to configured key, count=%d", poller.count())
	}
	if _, ok := w.lastPoll["Org/Repo"]; !ok || len(w.lastPoll) != 1 {
		t.Fatalf("lastPoll keys = %+v", w.lastPoll)
	}
}

func TestWebhookReceiverMethod(t *testing.T) {
	w := NewWebhookReceiver(func() string { return "secret" }, nil, time.Second, nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/github/mentions/webhook", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}
}

func TestWebhookReceiverEdgeCases(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"org/repo"}}`
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	w := NewWebhookReceiver(nil, nil, 0, logger)
	if w.minInterval != webhookDefaultMinGap {
		t.Fatalf("default min interval = %v", w.minInterval)
	}
	if !strings.Contains(logs.String(), "secret is not configured") {
		t.Fatalf("missing construction warning: %s", logs.String())
	}
	if w.verify([]byte(body), signGitHubWebhook(body, "secret")) {
		t.Fatal("nil secret verifier accepted signature")
	}

	w = NewWebhookReceiver(func() string { return " secret " }, nil, 0, nil)
	if w.verify([]byte(body), "sha256=not-hex") {
		t.Fatal("bad hex signature accepted")
	}
	if w.verify([]byte(body), "sha256=") {
		t.Fatal("empty signature accepted")
	}
	rec := postMentionWebhook(w, "issues", `{"action":"opened","repository":{"full_name":"org/repo"}}`, signGitHubWebhook(`{"action":"opened","repository":{"full_name":"org/repo"}}`, "secret"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("nil poller status = %d", rec.Code)
	}
	if repo, ok := webhookRepo("issues", []byte(`{"action":"opened","repository":{"full_name":"org/repo"}}`)); !ok || repo != "org/repo" {
		t.Fatalf("webhookRepo = %q/%v", repo, ok)
	}
}

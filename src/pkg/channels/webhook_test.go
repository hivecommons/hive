package channels

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func boolPtr(b bool) *bool { return &b }

// kickRecorder captures kicks so a test can assert which agents were triggered
// without a real agent runtime.
type kickRecorder struct {
	mu    sync.Mutex
	kicks []kickRecord
	err   error
}

type kickRecord struct {
	agent   string
	message string
}

func (k *kickRecorder) fn(agent, message string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kicks = append(k.kicks, kickRecord{agent: agent, message: message})
	return k.err
}

func (k *kickRecorder) snapshot() []kickRecord {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]kickRecord, len(k.kicks))
	copy(out, k.kicks)
	return out
}

// waitForKicks polls until n kicks have been recorded or the deadline passes.
// Webhook kicks are dispatched on their own goroutines, so a bare read races.
func (k *kickRecorder) waitForKicks(t *testing.T, n int) []kickRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := k.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return k.snapshot()
}

// webhookAgents builds an agent set with one webhook channel.
func webhookAgents(events, repos []string, enabled *bool) map[string]config.AgentConfig {
	return map[string]config.AgentConfig{
		"scanner": {
			Channels: []config.ChannelConfig{{
				Type:    "webhook",
				Events:  events,
				Repos:   repos,
				Enabled: enabled,
			}},
		},
	}
}

func postWebhook(t *testing.T, recv *WebhookReceiver, event string, body any, secret string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(raw)))
	if event != "" {
		req.Header.Set(webhookEventHeader, event)
	}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(raw)
		req.Header.Set(webhookSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	rec := httptest.NewRecorder()
	recv.ServeHTTP(rec, req)
	return rec
}

// ---- verifyHMAC ----

// The signature check is the only thing standing between the internet and a
// kick, so its rejection cases matter more than its accept case.
func TestVerifyHMAC(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	secret := "s3cr3t"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyHMAC(body, valid, secret) {
		t.Fatal("a correct signature must verify")
	}

	cases := []struct {
		name, sig string
	}{
		{"wrong secret's digest", "sha256=" + strings.Repeat("ab", 32)},
		{"missing sha256 prefix", strings.TrimPrefix(valid, "sha256=")},
		{"wrong algorithm prefix", strings.Replace(valid, "sha256=", "sha1=", 1)},
		{"not hex", "sha256=nothexatall"},
		{"empty", ""},
		{"truncated digest", valid[:len(valid)-4]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if verifyHMAC(body, tc.sig, secret) {
				t.Fatalf("signature %q must be rejected", tc.sig)
			}
		})
	}

	// A different body with an otherwise-valid signature must fail.
	if verifyHMAC([]byte(`{"action":"closed"}`), valid, secret) {
		t.Fatal("a signature must not verify against a different body")
	}
	// The right body under the wrong secret must fail.
	if verifyHMAC(body, valid, "different-secret") {
		t.Fatal("a signature must not verify under a different secret")
	}
}

// ---- ServeHTTP: auth and validation ----

// With a secret configured, an unsigned or wrongly-signed request must be
// rejected before any agent is kicked.
func TestWebhookRejectsBadSignatureWhenSecretSet(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	t.Run("no signature", func(t *testing.T) {
		w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "wrong-secret")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", w.Code)
		}
	})

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("an unauthorized webhook must not kick any agent, got %v", got)
	}
}

func TestWebhookRejectsMissingSecretWithActionableLog(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, logger)

	w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), webhookSecretEnvVar) {
		t.Fatalf("response should tell operators to configure %s, got %s", webhookSecretEnvVar, w.Body.String())
	}
	if !strings.Contains(logs.String(), webhookSecretEnvVar) {
		t.Fatalf("log should name missing %s, got %s", webhookSecretEnvVar, logs.String())
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a webhook without configured HMAC secret must not kick any agent, got %v", got)
	}
}

func TestWebhookAcceptsCorrectSignature(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := rec.waitForKicks(t, 1); len(got) != 1 {
		t.Fatalf("want 1 kick, got %v", got)
	}
}

func TestWebhookRejectsNonPost(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestWebhookRequiresEventHeader(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	w := postWebhook(t, m.WebhookHandler(), "", map[string]any{"action": "opened"}, "s3cr3t")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestWebhookRejectsInvalidJSON(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	raw := []byte("{not json")
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(raw)))
	req.Header.Set(webhookEventHeader, "issues")
	req.Header.Set(webhookSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// ---- ServeHTTP: routing ----

// A channel subscribed to the bare event type fires for any action.
func TestWebhookMatchesBareEventType(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	got := rec.waitForKicks(t, 1)
	if len(got) != 1 || got[0].agent != "scanner" {
		t.Fatalf("want scanner kicked once, got %v", got)
	}
	if !strings.Contains(w.Body.String(), `"triggered":1`) {
		t.Fatalf("response should report the trigger count: %s", w.Body.String())
	}
}

// A channel subscribed to "event.action" must fire only for that action, so a
// scanner watching issues.opened is not woken by every issue comment.
func TestWebhookMatchesQualifiedEventOnly(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues.opened"}, nil, nil), rec.fn, nil, quietLogger())

	if w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "closed"}, "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("issues.closed must not trigger an issues.opened channel, got %v", got)
	}

	if w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := rec.waitForKicks(t, 1); len(got) != 1 {
		t.Fatalf("issues.opened must trigger, got %v", got)
	}
}

// A repo allowlist confines a channel to its own repositories.
func TestWebhookHonoursRepoAllowlist(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, []string{"allowed-repo"}, nil), rec.fn, nil, quietLogger())

	body := map[string]any{
		"action":     "opened",
		"repository": map[string]any{"name": "other-repo"},
	}
	if w := postWebhook(t, m.WebhookHandler(), "issues", body, "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("an out-of-allowlist repo must not trigger, got %v", got)
	}

	body["repository"] = map[string]any{"name": "allowed-repo"}
	if w := postWebhook(t, m.WebhookHandler(), "issues", body, "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := rec.waitForKicks(t, 1); len(got) != 1 {
		t.Fatalf("an allowlisted repo must trigger, got %v", got)
	}
}

// A disabled channel must never fire.
func TestWebhookSkipsDisabledChannel(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, boolPtr(false)), rec.fn, nil, quietLogger())

	if w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a disabled channel must not trigger, got %v", got)
	}
}

// An event nobody subscribes to is accepted but triggers nothing.
func TestWebhookUnmatchedEventTriggersNothing(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	w := postWebhook(t, m.WebhookHandler(), "push", map[string]any{}, "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"triggered":0`) {
		t.Fatalf("want triggered:0, got %s", w.Body.String())
	}
}

// The trigger context handed to buildMsg must carry the event details, since
// that is what the agent's kick message is built from.
func TestWebhookTriggerContextCarriesEventDetail(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	var gotCtx TriggerContext
	var mu sync.Mutex
	buildMsg := func(agent string, ctx TriggerContext) string {
		mu.Lock()
		defer mu.Unlock()
		gotCtx = ctx
		return "msg for " + agent
	}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, buildMsg, quietLogger())

	body := map[string]any{
		"action":     "opened",
		"repository": map[string]any{"name": "my-repo"},
	}
	postWebhook(t, m.WebhookHandler(), "issues", body, "s3cr3t")
	kicks := rec.waitForKicks(t, 1)
	if len(kicks) != 1 {
		t.Fatalf("want 1 kick, got %v", kicks)
	}
	if kicks[0].message != "msg for scanner" {
		t.Fatalf("buildMsg result must reach the kick, got %q", kicks[0].message)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCtx.ChannelType != "webhook" {
		t.Errorf("channel type = %q", gotCtx.ChannelType)
	}
	if gotCtx.Event != "issues.opened" {
		t.Errorf("event = %q, want the qualified form", gotCtx.Event)
	}
	if gotCtx.Payload["repo"] != "my-repo" {
		t.Errorf("payload repo = %q", gotCtx.Payload["repo"])
	}
	if gotCtx.Payload["action"] != "opened" {
		t.Errorf("payload action = %q", gotCtx.Payload["action"])
	}
}

// ---- matchesEvent / containsStr ----

func TestMatchesEvent(t *testing.T) {
	cases := []struct {
		name      string
		events    []string
		eventType string
		fullEvent string
		want      bool
	}{
		{"bare type matches", []string{"issues"}, "issues", "issues.opened", true},
		{"qualified matches", []string{"issues.opened"}, "issues", "issues.opened", true},
		{"qualified does not match other action", []string{"issues.opened"}, "issues", "issues.closed", false},
		{"unrelated event", []string{"push"}, "issues", "issues.opened", false},
		{"empty subscription list", nil, "issues", "issues.opened", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesEvent(tc.events, tc.eventType, tc.fullEvent); got != tc.want {
				t.Fatalf("matchesEvent(%v,%q,%q) = %v, want %v",
					tc.events, tc.eventType, tc.fullEvent, got, tc.want)
			}
		})
	}
}

func TestContainsStr(t *testing.T) {
	if !containsStr([]string{"a", "b"}, "b") {
		t.Error("present value must be found")
	}
	if containsStr([]string{"a", "b"}, "c") {
		t.Error("absent value must not be found")
	}
	if containsStr(nil, "a") {
		t.Error("an empty slice contains nothing")
	}
}

package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

const testHiveAppBotLogin = "hive[bot]"

type levelHoldServer struct {
	mu            sync.Mutex
	comments      []string
	commentAuthor string
	labelAuthor   string
	pullBody      string
	issueAuthor   string
	issueAssoc    string
	removes       int
	releaseNotes  int
	reporterNotes int
	labelsAdded   []string
}

func (s *levelHoldServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			body := s.pullBody
			if body == "" {
				body = "safe change"
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"title":  "fix",
				"body":   body,
				"labels": []map[string]string{{"name": "hold"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			author := s.commentAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			out := make([]map[string]any, 0, len(s.comments))
			for i, body := range s.comments {
				out = append(out, map[string]any{"id": i + 1, "body": body, "user": map[string]string{"login": author}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			body, _ := io.ReadAll(r.Body)
			var payload gh.IssueComment
			_ = json.Unmarshal(body, &payload)
			s.comments = append(s.comments, payload.GetBody())
			if strings.Contains(payload.GetBody(), "hold released by operator") {
				s.releaseNotes++
			}
			if IsReporterTrustHoldNotice(payload.GetBody()) {
				s.reporterNotes++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/581":
			author := s.issueAuthor
			if author == "" {
				author = "alice"
			}
			assoc := s.issueAssoc
			if assoc == "" {
				assoc = "NONE"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":             581,
				"user":               map[string]string{"login": author, "type": "User"},
				"author_association": assoc,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			author := s.labelAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": author},
				"label":      map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/labels":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			s.labelsAdded = append(s.labelsAdded, labels...)
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			s.removes++
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newLevelHoldClient(t *testing.T, s *levelHoldServer) *Client {
	t.Helper()
	c := NewClientForTest(s.start(t).URL, "acme/widget", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	return c
}

func heldPRFixture() *gh.PullRequest {
	return &gh.PullRequest{Number: gh.Ptr(11), Title: gh.Ptr("fix"), Body: gh.Ptr("safe change"), Labels: []*gh.Label{{Name: gh.Ptr("hold")}}}
}

func TestSweepDoesNotReleaseLevelHoldAfterPromotion(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}}
	c := newLevelHoldClient(t, s)
	c.prHoldLabel = func(agent string) bool { return false }

	released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPRFixture())
	if err != nil || released || reason != "hold" {
		t.Fatalf("releaseLevelHoldIfEligible = (%v,%q,%v), want held/no release", released, reason, err)
	}
	if s.removes != 0 {
		t.Fatalf("removes=%d, want zero", s.removes)
	}
}

func TestReleaseLevelHoldDoesNotReleaseUnknownOrForgedHold(t *testing.T) {
	for _, tc := range []struct {
		name          string
		comments      []string
		commentAuthor string
		wantReason    string
	}{
		{name: "unknown", wantReason: "hold"},
		{name: "forged", comments: []string{levelHoldNotice("quality")}, commentAuthor: "alice", wantReason: "hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &levelHoldServer{comments: tc.comments, commentAuthor: tc.commentAuthor}
			c := newLevelHoldClient(t, s)
			c.prHoldLabel = func(agent string) bool { return false }
			released, reason, err := c.releaseLevelHoldIfEligible(context.Background(), "acme", "widget", heldPRFixture())
			if err != nil || released || reason != tc.wantReason || s.removes != 0 {
				t.Fatalf("release=(%v,%q,%v) removes=%d", released, reason, err, s.removes)
			}
		})
	}
}

func TestReleaseLevelHoldsOnceRemovesLevelHoldAndComments(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}}
	c := newLevelHoldClient(t, s)
	c.SetRepos([]string{"acme/widget"})

	released, err := c.ReleaseLevelHoldsOnce(context.Background(), 6, "owner")
	if err != nil {
		t.Fatalf("ReleaseLevelHoldsOnce: %v", err)
	}
	if len(released) != 1 || released[0].Repo != "acme/widget" || released[0].Number != 11 || released[0].Agent != "quality" {
		t.Fatalf("released = %+v, want acme/widget#11 quality", released)
	}
	if s.removes != 1 || s.releaseNotes != 1 {
		t.Fatalf("removes=%d releaseNotes=%d, want one each", s.removes, s.releaseNotes)
	}
	if got := s.comments[len(s.comments)-1]; !strings.Contains(got, "hold released by operator owner when raising to L6") {
		t.Fatalf("release comment = %q", got)
	}
}

func TestPendingLevelHoldsIgnoresHumanRelabel(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}, labelAuthor: "alice"}
	c := newLevelHoldClient(t, s)
	c.SetRepos([]string{"acme/widget"})

	if pending, err := c.PendingLevelHolds(context.Background()); err != nil {
		t.Fatalf("PendingLevelHolds: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("PendingLevelHolds after human relabel = %+v, want none", pending)
	}
}

func TestReleaseLevelHoldsOnceDoesNotReleaseReporterTrustHold(t *testing.T) {
	s := &levelHoldServer{comments: []string{levelHoldNotice("quality")}, pullBody: "Closes #581", issueAuthor: "stranger", issueAssoc: "NONE"}
	c := newLevelHoldClient(t, s)
	c.SetRepos([]string{"acme/widget"})
	c.SetReporterTrustHoldEnabled(func(string) bool { return true })
	c.SetReporterTrusted(func(login, association string) bool { return false })

	released, err := c.ReleaseLevelHoldsOnce(context.Background(), 6, "owner")
	if err != nil {
		t.Fatalf("ReleaseLevelHoldsOnce: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("released = %+v, want none for reporter-trust hold", released)
	}
	if s.removes != 0 || s.releaseNotes != 0 {
		t.Fatalf("removes=%d releaseNotes=%d, want no release", s.removes, s.releaseNotes)
	}
	if s.reporterNotes != 1 {
		t.Fatalf("reporterNotes=%d, want reporter-trust notice posted", s.reporterNotes)
	}
	if len(s.labelsAdded) != 1 || s.labelsAdded[0] != "needs-human" {
		t.Fatalf("labelsAdded=%v, want [needs-human] raised with the reporter-trust hold (#10773)", s.labelsAdded)
	}
}

func TestEnsureLevelHoldNoticePostsAgentAndDoesNotDuplicate(t *testing.T) {
	s := &levelHoldServer{}
	c := newLevelHoldClient(t, s)

	if err := c.ensureLevelHoldNotice(context.Background(), "acme/widget", 11, "quality"); err != nil {
		t.Fatalf("first ensureLevelHoldNotice: %v", err)
	}
	if err := c.ensureLevelHoldNotice(context.Background(), "acme/widget", 11, "quality"); err != nil {
		t.Fatalf("second ensureLevelHoldNotice: %v", err)
	}
	if len(s.comments) != 1 {
		t.Fatalf("posted %d comments, want one", len(s.comments))
	}
	for _, want := range []string{levelHoldNoticePrefix, `"agent":"quality"`, "quality", "human removes"} {
		if !strings.Contains(s.comments[0], want) {
			t.Fatalf("level hold notice missing %q:\n%s", want, s.comments[0])
		}
	}
}

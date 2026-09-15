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

type levelHoldSweepServer struct {
	mu            sync.Mutex
	held          bool
	comments      []string
	commentAuthor string
	labelAuthor   string
	prBody        string
	rationale     *selfAuthIssue
	removes       int
	merges        int
}

func (s *levelHoldSweepServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11,
				"draft":  false,
				"user":   map[string]string{"login": testHiveAppBotLogin},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/11":
			labels := []map[string]string{}
			s.mu.Lock()
			if s.held {
				labels = append(labels, map[string]string{"name": "hold"})
			}
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"number":          11,
				"title":           "fix",
				"body":            s.body(),
				"state":           "open",
				"draft":           false,
				"mergeable_state": "clean",
				"mergeable":       true,
				"user":            map[string]string{"login": testHiveAppBotLogin},
				"head":            map[string]string{"sha": "sha11"},
				"base":            map[string]string{"ref": "main"},
				"labels":          labels,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			s.mu.Lock()
			out := make([]map[string]any, 0, len(s.comments))
			author := s.commentAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			for _, body := range s.comments {
				out = append(out, map[string]any{"body": body, "user": map[string]string{"login": author}})
			}
			s.mu.Unlock()
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/11/comments":
			body, _ := io.ReadAll(r.Body)
			var payload gh.IssueComment
			_ = json.Unmarshal(body, &payload)
			s.mu.Lock()
			s.comments = append(s.comments, payload.GetBody())
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/581":
			if s.rationale == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number":   581,
				"user":     userJSON(s.rationale.Author, s.rationale.AuthorType),
				"labels":   []map[string]any{},
				"comments": len(s.rationale.Comments),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/581/comments":
			if s.rationale == nil || s.rationale.FailList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := make([]map[string]any, 0, len(s.rationale.Comments))
			for _, c := range s.rationale.Comments {
				out = append(out, map[string]any{"user": userJSON(c.Author, c.AuthorType)})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/acme/widget/issues/11/labels/hold":
			s.mu.Lock()
			s.removes++
			s.held = false
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/11/events":
			s.mu.Lock()
			author := s.labelAuthor
			if author == "" {
				author = testHiveAppBotLogin
			}
			s.mu.Unlock()
			json.NewEncoder(w).Encode([]map[string]any{{
				"event":      "labeled",
				"created_at": "2026-09-15T12:00:00Z",
				"actor":      map[string]string{"login": author},
				"label":      map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha11/status":
			json.NewEncoder(w).Encode(map[string]any{
				"state":       "success",
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": "success"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha11/check-runs":
			json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/11/merge":
			s.mu.Lock()
			s.merges++
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "merge11"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *levelHoldSweepServer) body() string {
	if s.prBody != "" {
		return s.prBody
	}
	return "safe change"
}

func TestSweepSelfAuthoredAutoMergesReleasesLevelHoldAfterPromotion(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{levelHoldNotice("quality")}}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }
	c.SetRequiredChecks(map[string]bool{"ci/build": true, "build": true})

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if result.Skipped != 1 || len(result.Merged) != 0 {
		t.Fatalf("result=%+v, want released hold to be skipped for this tick", result)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removes != 1 || s.held {
		t.Fatalf("removes=%d held=%v, want exactly one release", s.removes, s.held)
	}
}

func TestSweepSelfAuthoredAutoMergesDoesNotReleaseSelfAuthorizationHold(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{selfAuthorizationNotice(SelfAuthorization{
		Held: true, Repo: "acme/widget", Issue: 5117, Reason: "no human has acknowledged it",
	})}}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want self-authorization hold left in place", result, removes)
	}
}

func TestSweepSelfAuthoredAutoMergesKeepsSelfAuthorizationAfterLevelPromotion(t *testing.T) {
	s := &levelHoldSweepServer{
		held:      true,
		comments:  []string{levelHoldNotice("quality")},
		prBody:    "Closes #581",
		rationale: &selfAuthIssue{Author: testHiveAppBotLogin, AuthorType: "Bot"},
	}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	comments := append([]string(nil), s.comments...)
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want self-authorization to keep hold after level release check", result, removes)
	}
	if len(comments) != 2 || !strings.Contains(comments[1], "hivecommons/hive#5117") {
		t.Fatalf("comments=%q, want self-authorization notice added without releasing hold", comments)
	}
}

func TestSweepSelfAuthoredAutoMergesDoesNotReleaseUnknownHold(t *testing.T) {
	s := &levelHoldSweepServer{held: true}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want unknown/human hold left in place", result, removes)
	}
}

func TestSweepSelfAuthoredAutoMergesDoesNotReleaseForgedLevelHoldNotice(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{levelHoldNotice("quality")}, commentAuthor: "alice"}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want forged marker left fail-closed", result, removes)
	}
}

func TestSweepSelfAuthoredAutoMergesDoesNotReleaseHumanRelabeledHold(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{levelHoldNotice("quality")}, labelAuthor: "alice"}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want human-applied hold left fail-closed", result, removes)
	}
}

func TestSweepSelfAuthoredAutoMergesLevelHoldReleaseIsIdempotent(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{levelHoldNotice("quality")}}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return false }
	c.SetRequiredChecks(map[string]bool{"ci/build": true, "build": true})

	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removes != 1 {
		t.Fatalf("removes=%d, want one release across repeated sweeps", s.removes)
	}
	if s.merges != 1 {
		t.Fatalf("merges=%d, want second sweep to continue after the hold is gone", s.merges)
	}
}

func TestSweepSelfAuthoredAutoMergesOutreachLevelHoldStaysHeld(t *testing.T) {
	s := &levelHoldSweepServer{held: true, comments: []string{levelHoldNotice("outreach")}}
	c := newAutoMergeSweepClient(s.start(t).URL)
	c.prHoldLabel = func(agent string) bool { return strings.EqualFold(agent, "outreach") }

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	s.mu.Lock()
	removes := s.removes
	s.mu.Unlock()
	if result.Skipped != 1 || removes != 0 {
		t.Fatalf("result=%+v removes=%d, want outreach to stay held", result, removes)
	}
}

func TestEnsureLevelHoldNoticePostsAgentAndDoesNotDuplicate(t *testing.T) {
	var comments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42/comments":
			out := make([]map[string]any, 0, len(comments))
			for _, body := range comments {
				out = append(out, map[string]any{"body": body, "user": map[string]string{"login": testHiveAppBotLogin}})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/42/comments":
			body, _ := io.ReadAll(r.Body)
			var payload gh.IssueComment
			_ = json.Unmarshal(body, &payload)
			comments = append(comments, payload.GetBody())
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)

	if err := c.ensureLevelHoldNotice(context.Background(), "o/r", 42, "quality"); err != nil {
		t.Fatalf("first ensureLevelHoldNotice: %v", err)
	}
	if err := c.ensureLevelHoldNotice(context.Background(), "o/r", 42, "quality"); err != nil {
		t.Fatalf("second ensureLevelHoldNotice: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("posted %d comments, want one", len(comments))
	}
	for _, want := range []string{levelHoldNoticePrefix, `"agent":"quality"`, "quality", "ACMM L3"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("level hold notice missing %q:\n%s", want, comments[0])
		}
	}
}

package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// relayServer serves the status-comment fake and answers label calls itself.
func relayServer(t *testing.T, s *statusCommentServer) *httptest.Server {
	t.Helper()
	inner := s.start(t).Config.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/labels") {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"name":"x"}`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRelayCommentStateNoteFoldsUnderAttentionPilot(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		pilot                  bool
		wantCreates, wantEdits int
	}{
		{"pilot on", true, 1, 1},
		{"pilot off", false, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &statusCommentServer{}
			c := issueTestClient(t, relayServer(t, s).URL)
			c.SetAppBotLogin("hive-app[bot]")
			c.SetAttentionPilot(func() bool { return tc.pilot })
			dir := withIssueDir(t)
			for _, body := range []string{"hive/awaiting-ci: CI pending", "already fixed in #9"} {
				if _, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: body, Agent: "quality"}); err != nil {
					t.Fatal(err)
				}
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if s.creates != tc.wantCreates || s.edits != tc.wantEdits {
				t.Fatalf("creates=%d edits=%d, want %d/%d", s.creates, s.edits, tc.wantCreates, tc.wantEdits)
			}
			cards := 0
			for _, cm := range s.comments {
				if b, _ := cm["body"].(string); prstatus.IsStatusComment(b) {
					cards++
				}
			}
			if (cards == 1) != tc.pilot {
				t.Fatalf("status cards=%d with pilot=%v", cards, tc.pilot)
			}
		})
	}
}

func TestLabelDecisionDowngradeNoteFoldsUnderAttentionPilot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pilot bool
	}{{"pilot on", true}, {"pilot off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &statusCommentServer{}
			c := issueTestClient(t, relayServer(t, s).URL)
			c.SetAppBotLogin("hive-app[bot]")
			c.SetAttentionPilot(func() bool { return tc.pilot })
			dir := withIssueDir(t)
			if _, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "o/r", Number: 41, Agent: "quality",
				Labels: []string{c.needsDecisionLabel()},
				Body:   "Choose approach A vs B for config shape and scope",
			}); err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if len(s.comments) != 1 {
				t.Fatalf("comments=%d, want exactly one", len(s.comments))
			}
			body, _ := s.comments[0]["body"].(string)
			_, folded := prstatus.Get(body, prstatus.KeyIssueDecisionDowngrade)
			if folded != tc.pilot || prstatus.IsStatusComment(body) != tc.pilot {
				t.Fatalf("folded=%v pilot=%v body=%q", folded, tc.pilot, body)
			}
			if !tc.pilot && !strings.Contains(body, "Hive did not park this") {
				t.Fatalf("raw note missing: %q", body)
			}
		})
	}
}

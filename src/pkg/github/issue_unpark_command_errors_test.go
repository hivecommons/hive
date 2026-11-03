package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// unparkFailServer serves one parked issue and lets a test make any single
// endpoint fail, so the sweep's error branches can be exercised one at a time.
type unparkFailServer struct {
	failPath   string // substring of the request path that should fail
	failMethod string
	failStatus int
	issue      unparkWireIssue
	// pullRequest serves the issue with a pull_request block so it reads as a PR.
	pullRequest bool
	permission  string
	removed     []string
	added       []string
}

func newUnparkFailServer(t *testing.T, s *unparkFailServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/org/repo/")
		if s.failPath != "" && strings.Contains(path, s.failPath) && (s.failMethod == "" || r.Method == s.failMethod) {
			w.WriteHeader(s.failStatus)
			return
		}
		switch {
		case path == "issues" && r.Method == http.MethodGet:
			if s.pullRequest {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"number": s.issue.Number, "title": s.issue.Title, "labels": s.issue.Labels,
					"pull_request": map[string]any{"url": "https://example.test/pull/2"},
				}})
				return
			}
			_ = json.NewEncoder(w).Encode([]unparkWireIssue{s.issue})
		case strings.HasPrefix(path, "issues/") && !strings.Contains(strings.TrimPrefix(path, "issues/"), "/") && r.Method == http.MethodGet:
			// The stage engine re-reads the item before it acts on an
			// Unparked fact (enrichStageFacts).
			_ = json.NewEncoder(w).Encode(s.issue)
		case strings.HasSuffix(path, "/timeline") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(s.issue.Comments)
		case strings.HasSuffix(path, "/comments") && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001})
		case strings.HasPrefix(path, "issues/comments/") && r.Method == http.MethodPatch:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case strings.HasSuffix(path, "/labels") && r.Method == http.MethodPost:
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			s.added = append(s.added, labels...)
			_ = json.NewEncoder(w).Encode([]wireLabel{})
		case strings.Contains(path, "/labels/") && r.Method == http.MethodDelete:
			var n int
			var label string
			fmt.Sscanf(path, "issues/%d/labels/%s", &n, &label)
			s.removed = append(s.removed, label)
			_ = json.NewEncoder(w).Encode([]wireLabel{})
		case strings.HasPrefix(path, "collaborators/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": s.permission})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestSweepIssueUnparkCommandsNilClient(t *testing.T) {
	var c *Client
	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != ErrNoGitHubClient {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
}

func TestSweepIssueUnparkCommandsListIssuesError(t *testing.T) {
	s := &unparkFailServer{failPath: "issues", failMethod: http.MethodGet, failStatus: http.StatusInternalServerError,
		issue: parkedIssue(1), permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err == nil {
		t.Fatal("expected listing error to surface")
	}
}

func TestSweepIssueUnparkCommandsSkipsPullRequestsAndUnparked(t *testing.T) {
	s := &unparkFailServer{issue: parkedIssue(2), permission: "write", pullRequest: true}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 0 || len(result.Unparked) != 0 {
		t.Fatalf("pull request must be skipped, got %+v", result)
	}
}

func TestSweepIssueUnparkCommandsPerIssueErrorIsSkipped(t *testing.T) {
	// Comments listing fails: the issue is counted as skipped, not fatal.
	s := &unparkFailServer{failPath: "/comments", failMethod: http.MethodGet, failStatus: http.StatusBadGateway,
		issue: parkedIssue(3, humanComment(31, "maintainer", "/hive approve")), permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("per-issue error must not abort the sweep: %v", err)
	}
	if result.Seen != 1 || result.Skipped != 1 || len(result.Unparked) != 0 {
		t.Fatalf("expected 1 seen / 1 skipped, got %+v", result)
	}
}

func TestSweepIssueUnparkCommandsPermissionLookupErrorSkips(t *testing.T) {
	s := &unparkFailServer{failPath: "collaborators/", failStatus: http.StatusInternalServerError,
		issue: parkedIssue(4, humanComment(41, "maintainer", "/hive approve")), permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 || result.Skipped != 1 {
		t.Fatalf("permission failure must never un-park, got %+v", result)
	}
	if len(s.removed) != 0 {
		t.Fatalf("no labels may be touched, got %v", s.removed)
	}
}

func TestSweepIssueUnparkCommandsLabelRemovalFailureSkips(t *testing.T) {
	s := &unparkFailServer{failPath: "/labels/", failMethod: http.MethodDelete, failStatus: http.StatusForbidden,
		issue: parkedIssue(5, humanComment(51, "maintainer", "/hive approve")), permission: "admin"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 || result.Skipped != 1 {
		t.Fatalf("label failure must not count as un-parked, got %+v", result)
	}
	if len(s.added) != 0 {
		t.Fatalf("approval label must not be added when removal failed, got %v", s.added)
	}
}

func TestSweepIssueUnparkCommandsLabelAlreadyGoneIsFine(t *testing.T) {
	s := &unparkFailServer{failPath: "/labels/", failMethod: http.MethodDelete, failStatus: http.StatusNotFound,
		issue: parkedIssue(6, humanComment(61, "maintainer", "/hive approve")), permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 1 {
		t.Fatalf("a 404 on label removal must not block un-parking, got %+v", result)
	}
	if len(s.added) != 1 || s.added[0] != HumanAckLabel {
		t.Fatalf("expected %s added, got %v", HumanAckLabel, s.added)
	}
}

func TestSweepIssueUnparkCommandsLabelAddFailureSkips(t *testing.T) {
	s := &unparkFailServer{failPath: "/labels", failMethod: http.MethodPost, failStatus: http.StatusForbidden,
		issue: parkedIssue(7, humanComment(71, "maintainer", "/hive approve")), permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 || result.Skipped != 1 {
		t.Fatalf("add failure must not count as un-parked, got %+v", result)
	}
}

func TestSweepIssueUnparkCommandsReplyFailureSkips(t *testing.T) {
	for _, body := range []string{"/hive approve", "/hive help", "sounds good, go ahead"} {
		t.Run(body, func(t *testing.T) {
			s := &unparkFailServer{failPath: "/comments", failMethod: http.MethodPost, failStatus: http.StatusForbidden,
				issue: parkedIssue(8, humanComment(81, "maintainer", body)), permission: "write"}
			server := newUnparkFailServer(t, s)
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.Unparked) != 0 || result.Replies != 0 || result.Skipped != 1 {
				t.Fatalf("reply failure must not be recorded as an action, got %+v", result)
			}
		})
	}
}

func TestClearIssueParkingLabelsSkipsAbsentLabels(t *testing.T) {
	issue := parkedIssue(9)
	issue.Labels = []wireLabel{{Name: HumanAckLabel}}
	s := &unparkFailServer{issue: issue, permission: "write"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	removed, err := c.clearIssueParkingLabels(context.Background(), "org", "repo", 9, []string{HumanAckLabel}, "maintainer", false)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	// The Unparked fact still reaches the stage engine, which stages the
	// now-unparked issue 010 (hivecommons/hive#13075); no parking label
	// moves and nothing else is added.
	var extra []string
	for _, l := range s.added {
		if !stagelabels.IsStageLabel(strings.ToLower(l)) {
			extra = append(extra, l)
		}
	}
	if len(removed) != 0 || len(s.removed) != 0 || len(extra) != 0 {
		t.Fatalf("nothing to change, got removed=%v added=%v", s.removed, s.added)
	}
}

func TestCommenterMayUnparkEdgeCases(t *testing.T) {
	s := &unparkFailServer{issue: parkedIssue(10), permission: "triage"}
	server := newUnparkFailServer(t, s)
	c := newTestClient(t, server, "org", []string{"repo"})

	ok, err := c.commenterMayUnpark(context.Background(), "org", "repo", "  ", false)
	if err != nil || ok {
		t.Fatalf("blank login: ok=%v err=%v, want false/nil", ok, err)
	}
	ok, err = c.commenterMayUnpark(context.Background(), "org", "repo", "someone", false)
	if err != nil || ok {
		t.Fatalf("triage: ok=%v err=%v, want false/nil", ok, err)
	}

	s.failPath = "collaborators/"
	s.failStatus = http.StatusInternalServerError
	ok, err = c.commenterMayUnpark(context.Background(), "org", "repo", "someone", false)
	if err == nil || ok {
		t.Fatalf("lookup failure must be an error, not permission: ok=%v err=%v", ok, err)
	}
}

func TestLabelPresent(t *testing.T) {
	labels := []string{" Needs-Human ", "hold"}
	if !labelPresent(labels, "needs-human") {
		t.Fatal("expected case/space-insensitive match")
	}
	if labelPresent(labels, "needs-decision") {
		t.Fatal("unexpected match")
	}
}

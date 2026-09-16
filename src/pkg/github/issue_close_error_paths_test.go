package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// These tests cover the error and guard branches of the reporter-confirmation
// close gate (CloseIssue, hasReporterConfirmationRequest) and its shared
// predicates in pr_request_claims.go, added in the #7125 fixes. The happy
// paths are pinned by issue_close_test.go; here every API failure arm and
// nil-guard is exercised so a regression in error wrapping or fail-open
// behaviour is caught.

func TestCloseIssueGuardBranches(t *testing.T) {
	ctx := context.Background()

	var nilClient *Client
	if err := nilClient.CloseIssue(ctx, "r", 7, IssueCloseOptions{}); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil *Client: err = %v, want ErrNoGitHubClient", err)
	}
	if err := (&Client{}).CloseIssue(ctx, "r", 7, IssueCloseOptions{}); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil inner client: err = %v, want ErrNoGitHubClient", err)
	}

	// A server that fails the test if any request gets through the guards.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request past guard: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	if err := c.CloseIssue(ctx, "<REPO>", 7, IssueCloseOptions{}); err == nil || !strings.Contains(err.Error(), "CloseIssue:") {
		t.Errorf("invalid repo ref: err = %v, want CloseIssue-wrapped validation error", err)
	}
	if err := c.CloseIssue(ctx, "r", 0, IssueCloseOptions{}); err == nil || !strings.Contains(err.Error(), "issue number is required") {
		t.Errorf("zero issue number: err = %v, want issue-number error", err)
	}
}

func TestCloseIssueGetFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})
	if err == nil || !strings.Contains(err.Error(), "before close") {
		t.Fatalf("err = %v, want read-before-close error", err)
	}
}

// closeGateMux serves a gated (human-filed bug) issue and delegates the
// comment/edit endpoints to the given handlers so each test can fail exactly
// one arm.
func closeGateMux(t *testing.T, issue *gh.Issue, listComments, createComment, edit http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(issue)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			listComments(w, r)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			createComment(w, r)
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			edit(w, r)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func emptyCommentList(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("[]"))
}

func failHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
}

func TestCloseIssueConfirmationCommentFailureStillGates(t *testing.T) {
	issue := closeGateIssue("human", "User", "Bug: broken", "reported by a person", []string{"bug"})
	srv := closeGateMux(t, issue, emptyCommentList, failHandler, failHandler)
	defer srv.Close()
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})
	if !errors.Is(err, ErrReporterConfirmationRequired) {
		t.Fatalf("err = %v, want ErrReporterConfirmationRequired", err)
	}
	if !strings.Contains(err.Error(), "failed to post confirmation request") {
		t.Fatalf("err = %v, want comment-failure detail appended", err)
	}
}

func TestCloseIssueOverrideCommentFailureAbortsClose(t *testing.T) {
	issue := closeGateIssue("human", "User", "Bug: dup", "reported by a person", []string{"bug"})
	var closed bool
	srv := closeGateMux(t, issue, emptyCommentList, failHandler,
		func(w http.ResponseWriter, r *http.Request) { closed = true },
	)
	defer srv.Close()
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{OverrideReason: "duplicate of #99"})
	if err == nil || !strings.Contains(err.Error(), "posting reporter-confirmation override") {
		t.Fatalf("err = %v, want override-comment failure", err)
	}
	if closed {
		t.Fatal("issue was closed even though the override audit comment failed to post")
	}
}

func TestCloseIssueEditFailure(t *testing.T) {
	issue := closeGateIssue("hive-app[bot]", "Bot", "Bug: bot finding", "automated", []string{"bug"})
	srv := closeGateMux(t, issue, emptyCommentList, failHandler, failHandler)
	defer srv.Close()
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})
	if err == nil || !strings.Contains(err.Error(), "closing issue") {
		t.Fatalf("err = %v, want close-edit failure", err)
	}
}

// TestHasReporterConfirmationRequestPaginates pins the pagination arm: a
// confirmation request posted 100+ comments ago (page 2) must still be found,
// so the gate does not re-notify the reporter.
func TestHasReporterConfirmationRequestPaginates(t *testing.T) {
	issue := closeGateIssue("human", "User", "Bug: old thread", "reported by a person", []string{"bug"})
	var posted int
	var srvURL string
	srv := closeGateMux(t, issue,
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 2, "body": reporterConfirmationRequestMarker + " please confirm"},
				})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/7/comments?page=2>; rel="next"`, srvURL))
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "unrelated"}})
		},
		func(w http.ResponseWriter, r *http.Request) { posted++ },
		failHandler,
	)
	defer srv.Close()
	srvURL = srv.URL
	c := testClient(t, srv.URL)

	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})
	if !errors.Is(err, ErrReporterConfirmationRequired) {
		t.Fatalf("err = %v, want ErrReporterConfirmationRequired", err)
	}
	if posted != 0 {
		t.Fatalf("posted %d new confirmation requests, want 0 (request found on page 2)", posted)
	}
}

func TestReporterConfirmationPredicatesNilAndLabel(t *testing.T) {
	if IsHumanFiledBugReport(nil) {
		t.Error("IsHumanFiledBugReport(nil) = true, want false")
	}
	if hasReporterConfirmation(nil) {
		t.Error("hasReporterConfirmation(nil) = true, want false")
	}
	// Label match is case-insensitive and whitespace-tolerant.
	labeled := closeGateIssue("human", "User", "Bug", "no marker in body", []string{" Hive: Reporter-Confirmed "})
	if !hasReporterConfirmation(labeled) {
		t.Error("hasReporterConfirmation(label variant) = false, want true")
	}
}

func TestIncompleteIssueReasonEpicTitlePrefix(t *testing.T) {
	for _, title := range []string{"[epic] big multi-phase plan", "[Tracker] rollout checklist"} {
		issue := &gh.Issue{Title: gh.Ptr(title), Body: gh.Ptr("plain body")}
		if got := incompleteIssueReason(issue); got != "issue title marks it as a tracker or epic" {
			t.Errorf("incompleteIssueReason(%q) = %q, want epic/tracker title reason", title, got)
		}
	}
}

func TestValidatePRRequestClaimsNilClient(t *testing.T) {
	if _, _, err := (&Client{}).validatePRRequestClaims(context.Background(), PRRequest{}); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
}

func TestValidatePRRequestClaimsDefaultBranchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	// Title claims a test artifact and Base is empty, so the default branch
	// must be resolved first — and its failure must surface, not be masked.
	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "quality/fix", Title: "[quality] add tests", Body: "adds tests",
	})
	if err == nil || !strings.Contains(err.Error(), "validating PR title artifacts") {
		t.Fatalf("err = %v, want title-artifact validation failure", err)
	}
}

func TestValidatePRRequestClaimsClosingRefGetFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/5":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)

	// No artifact claims in the title, so the only API call is the closing
	// reference lookup — whose failure must abort the PR request loudly.
	_, _, err := c.validatePRRequestClaims(context.Background(), PRRequest{
		Repo: "o/r", Head: "quality/fix", Title: "[quality] fix: thing", Body: "Closes #5",
	})
	if err == nil || !strings.Contains(err.Error(), "validating closing reference") {
		t.Fatalf("err = %v, want closing-reference validation failure", err)
	}
}

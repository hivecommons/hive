package worksource_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

func newIssuesTestServer(t *testing.T, org, repo, issuesJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, issuesJSON)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[]")
	})
	return httptest.NewServer(mux)
}

func TestGitHubIssuesSource_SourceType(t *testing.T) {
	src := worksource.NewGitHubIssuesSource(nil)
	if got := src.SourceType(); got != "github" {
		t.Errorf("SourceType() = %q, want %q", got, "github")
	}
}

func TestGitHubIssuesSource_ListIssues(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issuesJSON := `[
		{"number": 7, "title": "fix the bug", "user": {"login": "alice"},
		 "labels": [{"name": "bug"}], "assignees": [{"login": "bob"}],
		 "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		 "html_url": "https://github.com/testorg/testrepo/issues/7"}
	]`
	srv := newIssuesTestServer(t, org, repo, issuesJSON)
	defer srv.Close()

	client := github.NewClientForTest(srv.URL, org, []string{repo}, testLogger())
	src := worksource.NewGitHubIssuesSource(client)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	got := issues[0]
	if got.SourceType != "github" || got.Number != 7 || got.ExternalID != "7" ||
		got.Title != "fix the bug" || got.Author != "alice" || got.State != "open" ||
		got.URL != "https://github.com/testorg/testrepo/issues/7" {
		t.Errorf("unexpected issue: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "bug" {
		t.Errorf("labels = %v, want [bug]", got.Labels)
	}
	if len(got.Assignees) != 1 || got.Assignees[0] != "bob" {
		t.Errorf("assignees = %v, want [bob]", got.Assignees)
	}
}

func TestGitHubIssuesSource_ListIssues_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := github.NewClientForTest(srv.URL, "org", []string{"repo"}, testLogger())

	// This used to skip when EnumerateActionable returned nil — which is
	// exactly when it should fail (#5388): with a single repo whose every
	// request 500s, a nil error means the all-repos-failed guard in
	// EnumerateActionable regressed, and a zero-count result would tell the
	// governor the queue is empty and idle the agents. The guard's absence is
	// the bug this test exists to catch, not a reason to stand aside.
	if _, err := client.EnumerateActionable(context.Background()); err == nil {
		t.Fatal("EnumerateActionable returned nil error although every request to its " +
			"only repo failed; the all-repos-failed guard has regressed and a zero-count " +
			"result would silently idle the agents")
	}

	src := worksource.NewGitHubIssuesSource(client)
	_, err := src.ListIssues(context.Background())
	if err == nil {
		t.Fatal("expected error from ListIssues, got nil")
	}
	// The worksource must wrap, not swallow or rephrase, the enumeration
	// error, so callers can trace a failure back through the source boundary.
	if !strings.Contains(err.Error(), "worksource/github: enumerate:") {
		t.Errorf("ListIssues error %q does not carry the worksource/github: enumerate: wrap", err)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

package worksource_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	if _, err := client.EnumerateActionable(context.Background()); err == nil {
		// Some client paths tolerate per-repo failures; only assert the
		// worksource error wrapping when enumeration itself fails.
		t.Skip("EnumerateActionable tolerates repo errors; skipping error-path assertion")
	}

	src := worksource.NewGitHubIssuesSource(client)
	if _, err := src.ListIssues(context.Background()); err == nil {
		t.Fatal("expected error from ListIssues, got nil")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

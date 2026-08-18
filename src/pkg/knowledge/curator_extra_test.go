package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func curatorTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewCurator(t *testing.T) {
	ghClient := gh.NewClient(nil)
	c := NewCurator(ghClient, "http://wiki.test", "myorg", []string{"repo1", "repo2"},
		CuratorConfig{ExtractFrom: []string{"review_comments"}}, curatorTestLogger())
	if c == nil {
		t.Fatal("NewCurator returned nil")
	}
	if c.org != "myorg" {
		t.Errorf("org = %q", c.org)
	}
	if len(c.repos) != 2 {
		t.Errorf("repos = %d", len(c.repos))
	}
}

func TestRunExtraction_NoRepos(t *testing.T) {
	ghClient := gh.NewClient(nil)
	c := NewCurator(ghClient, "http://wiki.test", "myorg", nil,
		CuratorConfig{}, curatorTestLogger())

	facts, err := c.RunExtraction(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RunExtraction: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("facts = %d, want 0", len(facts))
	}
}

func TestRunExtraction_WithMergedPRs(t *testing.T) {
	now := time.Now()
	mergedAt := &gh.Timestamp{Time: now.Add(-1 * time.Hour)}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls", func(w http.ResponseWriter, r *http.Request) {
		prs := []map[string]interface{}{
			{
				"number":    1,
				"merged_at": mergedAt.Time.Format(time.RFC3339),
				"title":     "Fix bug",
			},
		}
		json.NewEncoder(w).Encode(prs)
	})
	mux.HandleFunc("/repos/myorg/repo1/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []map[string]interface{}{
			{"body": "Always use the pattern for error handling in this codebase, never skip it"},
		}
		json.NewEncoder(w).Encode(comments)
	})
	mux.HandleFunc("/repos/myorg/repo1/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []map[string]interface{}{
			{"body": "This was a regression from the last release and broke the build"},
		}
		json.NewEncoder(w).Encode(comments)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", []string{"repo1"},
		CuratorConfig{ExtractFrom: []string{"review_comments", "pr_comments"}}, curatorTestLogger())

	facts, err := c.RunExtraction(context.Background(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RunExtraction: %v", err)
	}
	if len(facts) < 1 {
		t.Errorf("expected at least 1 fact, got %d", len(facts))
	}
}

func TestRunExtraction_FullRepoPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/other-org/other-repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]interface{}{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	// Repo with full path "other-org/other-repo"
	c := NewCurator(client, "http://wiki.test", "myorg", []string{"other-org/other-repo"},
		CuratorConfig{}, curatorTestLogger())

	facts, err := c.RunExtraction(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RunExtraction: %v", err)
	}
	if facts != nil && len(facts) != 0 {
		t.Errorf("facts = %d", len(facts))
	}
}

func TestRunExtraction_FetchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", []string{"repo1"},
		CuratorConfig{}, curatorTestLogger())

	// Should not error out, just warn and continue
	facts, err := c.RunExtraction(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("RunExtraction should not return error: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("facts = %d, want 0", len(facts))
	}
}

func TestIngest_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ingest" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c := NewCurator(gh.NewClient(nil), server.URL, "myorg", nil,
		CuratorConfig{}, curatorTestLogger())

	facts := []ExtractedFact{
		{Title: "Test", Body: "Body", Type: FactGotcha, Confidence: 0.8},
	}
	err := c.Ingest(context.Background(), facts)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
}

func TestIngest_Empty(t *testing.T) {
	c := NewCurator(gh.NewClient(nil), "http://wiki.test", "myorg", nil,
		CuratorConfig{}, curatorTestLogger())
	err := c.Ingest(context.Background(), nil)
	if err != nil {
		t.Fatalf("Ingest(nil): %v", err)
	}
}

func TestIngest_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewCurator(gh.NewClient(nil), server.URL, "myorg", nil,
		CuratorConfig{}, curatorTestLogger())

	facts := []ExtractedFact{{Title: "Test", Body: "Body"}}
	err := c.Ingest(context.Background(), facts)
	if err == nil {
		t.Error("expected error for server error")
	}
}

func TestFetchReviewComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []map[string]interface{}{
			{"body": "Short"},
			{"body": "This is a long enough comment to pass the minimum length filter for extraction"},
		}
		json.NewEncoder(w).Encode(comments)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	comments := c.fetchReviewComments(context.Background(), "myorg", "repo1", 1)
	if len(comments) != 1 {
		t.Errorf("comments = %d, want 1 (short comment filtered)", len(comments))
	}
}

func TestFetchReviewComments_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	comments := c.fetchReviewComments(context.Background(), "myorg", "repo1", 1)
	if comments != nil {
		t.Errorf("expected nil on error, got %d comments", len(comments))
	}
}

func TestFetchIssueComments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []map[string]interface{}{
			{"body": "This is a sufficiently long issue comment for extraction purposes"},
		}
		json.NewEncoder(w).Encode(comments)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	comments := c.fetchIssueComments(context.Background(), "myorg", "repo1", 1)
	if len(comments) != 1 {
		t.Errorf("comments = %d, want 1", len(comments))
	}
}

func TestFetchIssueComments_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	comments := c.fetchIssueComments(context.Background(), "myorg", "repo1", 1)
	if comments != nil {
		t.Errorf("expected nil on error")
	}
}

func TestShouldExtractFrom_Multiple(t *testing.T) {
	c := &Curator{config: CuratorConfig{ExtractFrom: []string{"review_comments", "pr_comments"}}}
	if !c.shouldExtractFrom("review_comments") {
		t.Error("should extract from review_comments")
	}
	if !c.shouldExtractFrom("pr_comments") {
		t.Error("should extract from pr_comments")
	}
	if c.shouldExtractFrom("code_diff") {
		t.Error("should not extract from code_diff")
	}
}

func TestExtractFromPR(t *testing.T) {
	now := time.Now()
	mergedAt := &gh.Timestamp{Time: now.Add(-1 * time.Hour)}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls/42/comments", func(w http.ResponseWriter, r *http.Request) {
		comments := []map[string]interface{}{
			{"body": "You must always handle errors properly in this codebase"},
		}
		json.NewEncoder(w).Encode(comments)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil,
		CuratorConfig{ExtractFrom: []string{"review_comments"}}, curatorTestLogger())

	pr := &gh.PullRequest{
		Number:   gh.Ptr(42),
		MergedAt: mergedAt,
	}
	facts := c.extractFromPR(context.Background(), "myorg", "repo1", pr)
	if len(facts) != 1 {
		t.Errorf("facts = %d, want 1", len(facts))
	}
}

func TestFetchMergedPRs_FiltersByDate(t *testing.T) {
	now := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls", func(w http.ResponseWriter, r *http.Request) {
		prs := []map[string]interface{}{
			{
				"number":    1,
				"merged_at": now.Add(-1 * time.Hour).Format(time.RFC3339),
			},
			{
				"number":    2,
				"merged_at": now.Add(-48 * time.Hour).Format(time.RFC3339),
			},
			{
				"number": 3,
				// Not merged
			},
		}
		json.NewEncoder(w).Encode(prs)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	prs, err := c.fetchMergedPRs(context.Background(), "myorg", "repo1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("fetchMergedPRs: %v", err)
	}
	// Only PR #1 should pass (merged 1h ago, since is 24h ago)
	// PR #2 merged 48h ago should be filtered by the break
	if len(prs) != 1 {
		t.Errorf("prs = %d, want 1", len(prs))
	}
}

func TestFetchMergedPRs_UnmergedPRSkipped(t *testing.T) {
	now := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls", func(w http.ResponseWriter, r *http.Request) {
		// Unmerged PR comes first, then a merged one
		prs := []map[string]interface{}{
			{
				"number": 10,
				// no merged_at — unmerged PR
			},
			{
				"number":    11,
				"merged_at": now.Add(-1 * time.Hour).Format(time.RFC3339),
			},
		}
		json.NewEncoder(w).Encode(prs)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := ghClientFromServer(server)
	c := NewCurator(client, "http://wiki.test", "myorg", nil, CuratorConfig{}, curatorTestLogger())

	prs, err := c.fetchMergedPRs(context.Background(), "myorg", "repo1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("fetchMergedPRs: %v", err)
	}
	// PR #10 is unmerged (skipped), PR #11 is merged within 24h
	if len(prs) != 1 {
		t.Errorf("prs = %d, want 1", len(prs))
	}
}

func TestClassifyComment_HeadingSkipped(t *testing.T) {
	// Comment that starts with # should be skipped by extractTitle
	fact := classifyComment("# This is a heading\n", "org/repo#1", time.Now())
	if fact != nil {
		t.Error("expected nil fact for heading-only comment")
	}
}

func TestExtractTitle_CodeBlockSkipped(t *testing.T) {
	// extractTitle should skip lines starting with ```
	title := extractTitle("```\ncode block\n```\nActual title line here for extraction")
	if title == "```" {
		t.Error("extractTitle should skip ``` lines")
	}
}

func TestExtractTitle_HTMLCommentSkipped(t *testing.T) {
	title := extractTitle("<!-- comment -->\nActual content")
	if title == "<!-- comment -->" {
		t.Error("extractTitle should skip HTML comments")
	}
}

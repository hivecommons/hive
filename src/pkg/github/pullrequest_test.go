package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// prTestLogger returns a quiet logger for tests.
func prTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- CreatePR tests ---

func TestCreatePR_NilClient(t *testing.T) {
	var c *Client
	_, err := c.CreatePR(context.Background(), "org/repo", "feature", "main", "title", "body")
	if err != ErrNoGitHubClient {
		t.Fatalf("nil receiver: got %v, want ErrNoGitHubClient", err)
	}
}

func TestCreatePR_NilInnerClient(t *testing.T) {
	c := &Client{}
	_, err := c.CreatePR(context.Background(), "org/repo", "feature", "main", "title", "body")
	if err != ErrNoGitHubClient {
		t.Fatalf("nil inner client: got %v, want ErrNoGitHubClient", err)
	}
}

func TestCreatePR_EmptyHead(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "", "main", "title", "body")
	if err == nil || !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("empty head: got %v, want 'head branch is required'", err)
	}
}

func TestCreatePR_WhitespaceOnlyHead(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "   ", "main", "title", "body")
	if err == nil || !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("whitespace head: got %v, want 'head branch is required'", err)
	}
}

func TestCreatePR_EmptyTitle(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "feature", "main", "", "body")
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("empty title: got %v, want 'title is required'", err)
	}
}

func TestCreatePR_WhitespaceOnlyTitle(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "feature", "main", "   ", "body")
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("whitespace title: got %v, want 'title is required'", err)
	}
}

func TestCreatePR_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// DefaultBranch: GET /repos/org/repo
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/org/repo"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": "main"})
		// findOpenPRForHead: GET /repos/org/repo/pulls?head=org:feature&state=open
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]any{}) // no existing PR
		// CreatePR: POST /repos/org/repo/pulls
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["head"] != "feature" {
				t.Errorf("unexpected head: %s", body["head"])
			}
			if body["base"] != "main" {
				t.Errorf("unexpected base: %s", body["base"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"html_url": "https://github.com/org/repo/pull/42",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "repo", "feature", "", "Add tests", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Number != 42 {
		t.Errorf("number = %d, want 42", res.Number)
	}
	if res.URL != "https://github.com/org/repo/pull/42" {
		t.Errorf("url = %s", res.URL)
	}
	if res.AlreadyExisted {
		t.Error("AlreadyExisted should be false for a new PR")
	}
}

// TestCreatePR_EmptyBaseResolvesRepoDefault replaces the old
// TestCreatePR_DefaultBaseToMain: an empty base is no longer assumed to be
// "main", it is resolved from the repository's own default_branch metadata.
// This fixture's default happens to be "main" (the resolution path is what is
// under test here); kubestellar/hive#4928's actual regression coverage —
// a non-"main" default reaching the PR — lives in pullrequest_base_test.go.
func TestCreatePR_EmptyBaseResolvesRepoDefault(t *testing.T) {
	var receivedBase string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/org/repo"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": "main"})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]any{})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			receivedBase = body["base"]
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "https://example.com/pull/1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "feature", "", "title", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBase != "main" {
		t.Errorf("base = %q, want 'main'", receivedBase)
	}
}

func TestCreatePR_IdempotentReturnsExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 99, "html_url": "https://github.com/org/repo/pull/99"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "repo", "feature", "main", "title", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Number != 99 {
		t.Errorf("number = %d, want 99", res.Number)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted should be true when reusing existing PR")
	}
}

func TestCreatePR_OwnerRepoSplit(t *testing.T) {
	var receivedOwner, receivedRepo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path is /repos/{owner}/{repo}/pulls
		parts := strings.Split(r.URL.Path, "/")
		for i, p := range parts {
			if p == "repos" && i+2 < len(parts) {
				receivedOwner = parts[i+1]
				receivedRepo = parts[i+2]
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]any{})
		} else {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "url"})
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "default-org", nil, prTestLogger())

	// "custom-owner/custom-repo" should override the default org
	_, err := c.CreatePR(context.Background(), "custom-owner/custom-repo", "feat", "main", "t", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedOwner != "custom-owner" {
		t.Errorf("owner = %q, want 'custom-owner'", receivedOwner)
	}
	if receivedRepo != "custom-repo" {
		t.Errorf("repo = %q, want 'custom-repo'", receivedRepo)
	}
}

func TestCreatePR_BareRepoUsesClientOrg(t *testing.T) {
	var receivedOwner string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		for i, p := range parts {
			if p == "repos" && i+1 < len(parts) {
				receivedOwner = parts[i+1]
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]any{})
		} else {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "url"})
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "my-org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "my-repo", "feat", "main", "t", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedOwner != "my-org" {
		t.Errorf("owner = %q, want 'my-org'", receivedOwner)
	}
}

func TestCreatePR_Race422FallsBackToExisting(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			callCount++
			w.Header().Set("Content-Type", "application/json")
			if callCount == 1 {
				// First dedupe lookup: no PR exists yet
				json.NewEncoder(w).Encode([]any{})
			} else {
				// Second lookup after 422: the concurrent PR is now visible
				json.NewEncoder(w).Encode([]map[string]any{
					{"number": 77, "html_url": "https://github.com/org/repo/pull/77"},
				})
			}
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			// Simulate GitHub 422 for duplicate PR
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "Validation Failed",
				"errors":  []map[string]any{{"message": "A pull request already exists for org:feature"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "repo", "feature", "main", "title", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Number != 77 {
		t.Errorf("number = %d, want 77", res.Number)
	}
	if !res.AlreadyExisted {
		t.Error("AlreadyExisted should be true for 422 race recovery")
	}
}

func TestCreatePR_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]any{})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"message": "Internal Server Error"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.CreatePR(context.Background(), "repo", "feature", "main", "title", "body")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "creating PR") {
		t.Errorf("error should contain 'creating PR', got: %v", err)
	}
}

func TestCreatePR_DedupeLookupFailureStillCreates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			// Dedupe lookup fails with 500
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			// But creation succeeds
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 10, "html_url": "url/10"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	res, err := c.CreatePR(context.Background(), "repo", "feature", "main", "title", "body")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Number != 10 {
		t.Errorf("number = %d, want 10", res.Number)
	}
	if res.AlreadyExisted {
		t.Error("AlreadyExisted should be false for a new PR after failed dedupe")
	}
}

// --- findOpenPRForHead tests ---

func TestFindOpenPRForHead_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "open" {
			t.Errorf("state = %q, want 'open'", q.Get("state"))
		}
		if q.Get("head") != "org:feature" {
			t.Errorf("head = %q, want 'org:feature'", q.Get("head"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 55, "html_url": "https://github.com/org/repo/pull/55"},
		})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	pr, err := c.findOpenPRForHead(context.Background(), "org", "repo", "feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr == nil {
		t.Fatal("expected a PR, got nil")
	}
	if pr.GetNumber() != 55 {
		t.Errorf("number = %d, want 55", pr.GetNumber())
	}
}

func TestFindOpenPRForHead_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	pr, err := c.findOpenPRForHead(context.Background(), "org", "repo", "feature")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr != nil {
		t.Errorf("expected nil PR, got %+v", pr)
	}
}

func TestFindOpenPRForHead_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	_, err := c.findOpenPRForHead(context.Background(), "org", "repo", "feature")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

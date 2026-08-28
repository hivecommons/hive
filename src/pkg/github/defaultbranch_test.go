package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// repoMetadataServer serves GET /repos/{owner}/{repo} with the given
// default_branch and counts how many times it was asked.
func repoMetadataServer(t *testing.T, defaultBranch string, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/repos/org/repo") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "default_branch": defaultBranch})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDefaultBranch_ResolvesFromRepoMetadata asserts the invariant this bug
// broke: a repository whose default branch is NOT "main" resolves to that
// branch, not to an assumption. A test using a repo whose default is "main"
// would pass even with the old hardcoded behavior.
func TestDefaultBranch_ResolvesFromRepoMetadata(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "testing", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	got, err := c.DefaultBranch(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "testing" {
		t.Fatalf("DefaultBranch = %q, want %q — the repo's own default branch, not an assumption", got, "testing")
	}
}

func TestDefaultBranch_CachesResolvedBranch(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "develop", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	for i := 0; i < 3; i++ {
		got, err := c.DefaultBranch(context.Background(), "org", "repo")
		if err != nil {
			t.Fatalf("lookup %d: DefaultBranch: %v", i, err)
		}
		if got != "develop" {
			t.Fatalf("lookup %d: DefaultBranch = %q, want develop", i, got)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("repo metadata fetched %d times, want 1 — the resolved branch must be cached", n)
	}
}

func TestDefaultBranch_ErrorsWhenLookupFails(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	if _, err := c.DefaultBranch(context.Background(), "org", "repo"); err == nil {
		t.Fatal("DefaultBranch on API error = nil error, want a returned error (the caller decides fallback vs failure)")
	}
	// A failed lookup must NOT be cached: a transient 5xx would otherwise pin
	// this repo to a wrong answer for the rest of the process.
	if _, err := c.DefaultBranch(context.Background(), "org", "repo"); err == nil {
		t.Fatal("second lookup after a failure unexpectedly succeeded")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("repo metadata fetched %d times, want 2 — a failed lookup must not be cached", n)
	}
}

func TestDefaultBranch_EmptyDefaultBranchErrors(t *testing.T) {
	var calls int32
	srv := repoMetadataServer(t, "", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())

	if _, err := c.DefaultBranch(context.Background(), "org", "repo"); err == nil {
		t.Fatal("DefaultBranch with empty metadata = nil error, want an error rather than caching an empty base")
	}
}

func TestDefaultBranch_NilAndEmptyInputs(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.DefaultBranch(context.Background(), "org", "repo"); err == nil {
		t.Fatal("nil receiver: want an error, not a silent fallback")
	}
	if _, err := (&Client{}).DefaultBranch(context.Background(), "org", "repo"); err == nil {
		t.Fatal("nil inner client: want an error, not a silent fallback")
	}

	var calls int32
	srv := repoMetadataServer(t, "testing", &calls)
	c := NewClientForTest(srv.URL, "org", nil, prTestLogger())
	if _, err := c.DefaultBranch(context.Background(), "  ", "repo"); err == nil {
		t.Fatal("blank owner: want an error")
	}
	if _, err := c.DefaultBranch(context.Background(), "org", ""); err == nil {
		t.Fatal("blank repo: want an error")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("blank owner/repo issued %d API calls, want 0", n)
	}
}

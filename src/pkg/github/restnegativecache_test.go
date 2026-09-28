package github

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRESTNegativeCache_PullRequest404FetchedOncePerTTL(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	restore := resetREST404NegativeCacheForTest(func() time.Time { return now }, 30*time.Minute, 30*time.Minute)
	defer restore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/o/r/pulls/42" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := &http.Client{Transport: rest404NegativeCacheWrap(http.DefaultTransport)}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/pulls/42", nil)
		req = req.WithContext(WithRESTCaller(req.Context(), "hive:test_loop"))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d status = %d", i, resp.StatusCode)
		}
	}
	if requests != 1 {
		t.Fatalf("server requests before TTL = %d, want 1", requests)
	}

	now = now.Add(31 * time.Minute)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/pulls/42", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if requests != 2 {
		t.Fatalf("server requests after TTL = %d, want 2", requests)
	}
}

func TestRESTNegativeCache_ClaudeSettings404FetchedOnce(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	restore := resetREST404NegativeCacheForTest(func() time.Time { return now }, 30*time.Minute, 30*time.Minute)
	defer restore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !strings.HasSuffix(r.URL.Path, "/contents/.claude/settings.json") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: rest404NegativeCacheWrap(http.DefaultTransport)}
	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL + "/repos/o/r/contents/.claude/settings.json")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if requests != 1 {
		t.Fatalf("server requests = %d, want 1", requests)
	}
}

func TestRESTNegativeCache_IsScopedByAuthorization(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	restore := resetREST404NegativeCacheForTest(func() time.Time { return now }, 30*time.Minute, 30*time.Minute)
	defer restore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: rest404NegativeCacheWrap(http.DefaultTransport)}
	for _, token := range []string{"one", "one", "two"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/pulls/42", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if requests != 2 {
		t.Fatalf("server requests = %d, want one fetch per auth identity", requests)
	}
}

func TestRESTNegativeCache_MatchesGitHubEnterprisePathPrefix(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	restore := resetREST404NegativeCacheForTest(func() time.Time { return now }, 30*time.Minute, 30*time.Minute)
	defer restore()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: rest404NegativeCacheWrap(http.DefaultTransport)}
	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL + "/api/v3/repos/o/r/pulls/42")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if requests != 1 {
		t.Fatalf("server requests = %d, want GHE-prefixed PR 404 cached", requests)
	}
}

package github

import (
	"fmt"
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

func TestDurationFromEnv(t *testing.T) {
	const name = "HIVE_TEST_DURATION_FROM_ENV"
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", time.Minute},         // unset → fallback
		{"45m", 45 * time.Minute}, // Go duration string
		{"7", 7 * time.Minute},    // bare integer = minutes
		{"-5", time.Minute},       // negative integer → fallback
		{"garbage", time.Minute},  // unparsable → fallback
		{"-30m", time.Minute},     // negative duration → fallback
	}
	for _, tc := range cases {
		t.Setenv(name, tc.raw)
		if got := durationFromEnv(name, time.Minute); got != tc.want {
			t.Fatalf("durationFromEnv(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestRESTNegativeCache_PrunesExpiredAndEvictsOldest(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := &rest404NegativeCache{entries: map[string]rest404NegativeEntry{}, now: func() time.Time { return now }}

	// A zero/negative TTL is never cached.
	c.put("never", 0, "test")
	if len(c.entries) != 0 {
		t.Fatalf("zero-TTL put must be a no-op: %v", c.entries)
	}

	// An expired entry is pruned by the next put.
	c.put("stale", time.Second, "test")
	now = now.Add(2 * time.Second)
	c.put("fresh", time.Hour, "test")
	if _, ok := c.entries["stale"]; ok {
		t.Fatal("expired entry should have been pruned on the next put")
	}
	if _, ok := c.entries["fresh"]; !ok {
		t.Fatal("fresh entry missing after prune")
	}

	// Filling past the cap evicts the entries expiring soonest, never the
	// newest long-lived one.
	for i := 0; i < rest404NegativeCacheMaxEntries; i++ {
		c.put(fmt.Sprintf("k%05d", i), time.Duration(i+2)*time.Hour, "test")
	}
	if len(c.entries) != rest404NegativeCacheMaxEntries {
		t.Fatalf("cache size = %d, want cap %d", len(c.entries), rest404NegativeCacheMaxEntries)
	}
	if _, ok := c.entries["fresh"]; ok {
		t.Fatal("the soonest-expiring entry should have been evicted at the cap")
	}
	newest := fmt.Sprintf("k%05d", rest404NegativeCacheMaxEntries-1)
	if _, ok := c.entries[newest]; !ok {
		t.Fatalf("the longest-lived entry %s must survive eviction", newest)
	}
}

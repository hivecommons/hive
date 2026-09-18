package github

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ETagCacheStats is the operator's window into #7430: it must report the
// SHARED cache — the one etagCacheWrap installs in every client's transport
// chain — not a private store. Exercise the shared cache through the wrapper
// and assert the deltas show up.
func TestETagCacheStats_ReportsSharedCache(t *testing.T) {
	srv := &etagServer{body: `[{"number":7}]`, etag: `W/"stats-probe"`, remaining: 100}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	url := ts.URL + "/repos/hivecommons/hive/issues?stats-probe=1"
	// Drop this test's entry from the process-wide cache afterwards.
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer tok-stats")
		sharedETagCache.drop(etagCacheKey(req))
	})

	hits0, misses0, entries0 := ETagCacheStats()

	tr := etagCacheWrap(http.DefaultTransport)

	// Charged fetch: one miss, one stored body.
	resp, _ := etagGet(t, tr, url, "tok-stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first fetch = %d, want 200", resp.StatusCode)
	}
	// Free revalidation: one hit.
	resp, _ = etagGet(t, tr, url, "tok-stats")
	if resp.StatusCode != http.StatusOK || resp.Header.Get(etagCacheHeader) != "revalidated" {
		t.Fatalf("second fetch = %d (%s=%q), want a replayed 200",
			resp.StatusCode, etagCacheHeader, resp.Header.Get(etagCacheHeader))
	}

	hits1, misses1, entries1 := ETagCacheStats()
	if hits1-hits0 != 1 {
		t.Errorf("hits delta = %d, want 1", hits1-hits0)
	}
	if misses1-misses0 != 1 {
		t.Errorf("misses delta = %d, want 1", misses1-misses0)
	}
	if entries1-entries0 != 1 {
		t.Errorf("entries delta = %d, want 1", entries1-entries0)
	}
}

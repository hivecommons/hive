package github

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The built-in bounds are the config package's defaults, so the Settings
// dialog's "0 restores the default" and the cache agree on what that is.
func TestETagCache_DefaultsMatchConfig(t *testing.T) {
	c := newETagCache()
	if c.maxBytes != config.DefaultGitHubETagCacheMaxMB<<20 {
		t.Fatalf("maxBytes = %d, want %d MiB", c.maxBytes, config.DefaultGitHubETagCacheMaxMB)
	}
	if c.maxEntries != config.DefaultGitHubETagCacheMaxEntries {
		t.Fatalf("maxEntries = %d, want %d", c.maxEntries, config.DefaultGitHubETagCacheMaxEntries)
	}
}

// Shrinking the bounds evicts least-recently-used entries at once; a
// non-positive bound restores the default.
func TestETagCache_SetLimitsEvictsDown(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("ETag", fmt.Sprintf(`"e%d"`, n))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"n":1}`)
	}))
	defer ts.Close()
	tr, cache := newETagTransport()
	for i := 0; i < 6; i++ {
		etagGet(t, tr, fmt.Sprintf("%s/r/%d", ts.URL, i), "t")
	}
	// Touch entry 0 so it is the most recently used and survives.
	etagGet(t, tr, fmt.Sprintf("%s/r/0", ts.URL), "t")
	if len(cache.entries) != 6 {
		t.Fatalf("entries = %d, want 6 before resize", len(cache.entries))
	}

	cache.setLimits(cache.maxBytes, 2)
	if len(cache.entries) != 2 {
		t.Fatalf("entries = %d, want 2 after shrinking maxEntries", len(cache.entries))
	}
	if cache.get(etagCacheKeyFor(t, ts.URL+"/r/0", "t")) == nil {
		t.Error("most recently used entry was evicted")
	}
	for i := 1; i <= 4; i++ {
		if cache.get(etagCacheKeyFor(t, fmt.Sprintf("%s/r/%d", ts.URL, i), "t")) != nil {
			t.Errorf("entry %d survived shrinking to 2", i)
		}
	}

	// Shrink by bytes: one body's worth keeps exactly one entry.
	cache.setLimits(len(`{"n":1}`), 0)
	if cache.maxEntries != etagCacheMaxEntries {
		t.Fatalf("maxEntries = %d, want default %d restored", cache.maxEntries, etagCacheMaxEntries)
	}
	if len(cache.entries) != 1 || cache.totalBytes != len(`{"n":1}`) {
		t.Fatalf("entries = %d bytes = %d after byte shrink, want 1 / %d", len(cache.entries), cache.totalBytes, len(`{"n":1}`))
	}

	cache.setLimits(0, 0)
	if cache.maxBytes != etagCacheMaxBytes || cache.maxEntries != etagCacheMaxEntries {
		t.Fatalf("setLimits(0, 0) = %d/%d, want defaults", cache.maxBytes, cache.maxEntries)
	}
}

// SetETagCacheLimits / ETagCacheUsage act on the shared cache every wrapped
// client uses, and the usage snapshot reflects the new bounds.
func TestSetETagCacheLimits_SharedCache(t *testing.T) {
	_, maxBytes0, maxEntries0 := ETagCacheUsage()
	t.Cleanup(func() { SetETagCacheLimits(maxBytes0, maxEntries0) })

	SetETagCacheLimits(3<<20, 77)
	_, maxBytes, maxEntries := ETagCacheUsage()
	if maxBytes != 3<<20 || maxEntries != 77 {
		t.Fatalf("ETagCacheUsage() bounds = %d/%d, want 3 MiB / 77", maxBytes, maxEntries)
	}
	info := ETagCacheInfoSnapshot()
	if info.MaxBytes != 3<<20 || info.MaxEntries != 77 {
		t.Fatalf("ETagCacheInfoSnapshot() bounds = %d/%d", info.MaxBytes, info.MaxEntries)
	}
	if info.Bytes < 0 || info.Entries < 0 {
		t.Fatalf("negative usage: %+v", info)
	}
}

// Bodies over etagCacheCompressMin are stored gzip-compressed — the byte
// bound counts compressed bytes — and replayed byte-for-byte.
func TestETagCache_CompressesLargeBodies(t *testing.T) {
	body := `[` + strings.Repeat(`{"number":7,"title":"the same issue again","state":"open"},`, 200) + `{}]`
	srv := &etagServer{body: body, etag: `W/"big"`, remaining: 100}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	tr, cache := newETagTransport()

	resp, got := etagGet(t, tr, ts.URL+"/repos/o/r/issues", "t")
	if resp.StatusCode != http.StatusOK || got != body {
		t.Fatalf("first fetch: %d, body mismatch=%v", resp.StatusCode, got != body)
	}
	e := cache.get(etagCacheKeyFor(t, ts.URL+"/repos/o/r/issues", "t"))
	if e == nil || !e.gzipped {
		t.Fatalf("large JSON body was not stored compressed: %+v", e != nil)
	}
	if e.size != len(body) || len(e.body) >= len(body) || cache.totalBytes != len(e.body) {
		t.Fatalf("size=%d stored=%d total=%d raw=%d", e.size, len(e.body), cache.totalBytes, len(body))
	}

	resp, got = etagGet(t, tr, ts.URL+"/repos/o/r/issues", "t")
	if resp.Header.Get(etagCacheHeader) != "revalidated" || got != body {
		t.Fatalf("replay: %s=%q, body mismatch=%v", etagCacheHeader, resp.Header.Get(etagCacheHeader), got != body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("replayed ContentLength = %d, want %d (uncompressed)", resp.ContentLength, len(body))
	}

	// A small body stays raw: gzip framing would only cost memory.
	small := &etagServer{body: `{}`, etag: `"s"`, remaining: 100}
	ts2 := httptest.NewServer(http.HandlerFunc(small.handler))
	defer ts2.Close()
	etagGet(t, tr, ts2.URL+"/x", "t")
	if e := cache.get(etagCacheKeyFor(t, ts2.URL+"/x", "t")); e == nil || e.gzipped {
		t.Fatalf("small body should be stored raw")
	}

	// A corrupt compressed body is dropped and refetched, never replayed.
	e.body = []byte("not gzip")
	resp, got = etagGet(t, tr, ts.URL+"/repos/o/r/issues", "t")
	if resp.Header.Get(etagCacheHeader) != "" || got != body {
		t.Fatalf("corrupt entry was replayed: %s=%q", etagCacheHeader, resp.Header.Get(etagCacheHeader))
	}
	if e2 := cache.get(etagCacheKeyFor(t, ts.URL+"/repos/o/r/issues", "t")); e2 == nil || !e2.gzipped || string(e2.body) == "not gzip" {
		t.Fatal("corrupt entry was not replaced by the refetched body")
	}
}

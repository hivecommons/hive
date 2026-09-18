package github

// Conditional GitHub requests (#7430).
//
// The projectbluefin spoke exhausted its App installation quota (6650/hr)
// within one second of every reset: 11 Issues.ListByRepo call sites and 10
// PullRequests.List sites across seven files each re-list the same 16 repos,
// paginated, every governor cycle — hundreds of requests for data that is
// almost always identical to the previous cycle's. Nothing in the client path
// sent If-None-Match, so every one of those requests was charged in full.
//
// GitHub does not charge a 304 Not Modified against the REST rate limit. This
// transport makes every GET conditional when it has seen the resource before:
// it remembers the ETag and body of each 200, sends If-None-Match on the next
// request for the same resource by the same identity, and on 304 hands the
// caller the remembered 200 — go-github and every consumer above it see an
// ordinary response and never know the round trip was free. Unchanged lists
// (the common case between two five-minute cycles) stop costing quota.
//
// It sits in the shared transport chain (githubTransportChain) so the App
// client, token clients and the JWT client all get it without touching a
// single call site.
//
// Scope, deliberately narrow:
//   - GET only, and only when the caller sent no conditional/range header of
//     its own;
//   - only JSON responses that carry an ETag and fit etagCacheMaxBody;
//   - keyed by the requesting IDENTITY (a hash of the Authorization header)
//     as well as URL and Accept, so a scoped agent token can never be served
//     a body the App fetched — GitHub answers 304 only to an identity that may
//     see the resource, and a fresh identity simply misses once.
//
// Rate-limit headers on a replayed response are the 304's, not the stale
// 200's, so go-github's own quota bookkeeping stays exact.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// etagCacheMaxBody bounds one cached body. A 100-item issue page is
	// ~150 KiB; 4 MiB leaves room for large PR lists without letting a
	// tarball or diff response into memory.
	etagCacheMaxBody = 4 << 20
	// etagCacheMaxEntries / etagCacheMaxBytes bound the whole cache; least
	// recently used entries are evicted past either.
	etagCacheMaxEntries = 4096
	etagCacheMaxBytes   = 64 << 20
	// etagCacheTTL is how long an entry may be reused for revalidation. An
	// ETag never goes stale on GitHub's side, but an entry nobody has asked
	// about in this long is just memory.
	etagCacheTTL = 6 * time.Hour
	// etagCacheHeader marks a replayed response so a debugging operator can
	// tell a free revalidation from a charged fetch.
	etagCacheHeader = "X-Hive-Cache"
)

type etagEntry struct {
	etag     string
	header   http.Header
	body     []byte
	storedAt time.Time
	lastUsed time.Time
}

// etagCache is the process-wide store. Separate from the transport wrapper
// for the same reason slowStartState is: the wrapper is rebuilt around
// whatever inner transport is current, the memory must persist.
type etagCache struct {
	mu         sync.Mutex
	entries    map[string]*etagEntry
	totalBytes int
	maxEntries int
	maxBytes   int
	maxBody    int
	ttl        time.Duration
	now        func() time.Time

	// Counters for tests and for the operator: every hit is a request GitHub
	// did not charge.
	hits, misses, stores atomic.Int64
}

func newETagCache() *etagCache {
	return &etagCache{
		entries:    map[string]*etagEntry{},
		maxEntries: etagCacheMaxEntries,
		maxBytes:   etagCacheMaxBytes,
		maxBody:    etagCacheMaxBody,
		ttl:        etagCacheTTL,
		now:        time.Now,
	}
}

// sharedETagCache is the store every wrapped client shares.
var sharedETagCache = newETagCache()

// ETagCacheStats reports how many requests were answered by a free 304
// revalidation, how many were charged fetches, and how many bodies are held.
func ETagCacheStats() (hits, misses, entries int64) {
	c := sharedETagCache
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), int64(n)
}

type etagCacheTransport struct {
	inner http.RoundTripper
	cache *etagCache
}

func etagCacheWrap(inner http.RoundTripper) http.RoundTripper {
	return &etagCacheTransport{inner: inner, cache: sharedETagCache}
}

// githubTransportChain is the set of process-wide behaviours every GitHub
// client's requests pass through, in order: pacing after a rate limit
// (slowstart.go) outermost, then conditional-request caching, then the shared
// socket-owning transport. Pacing sits outside the cache on purpose: a
// conditional request is free against the PRIMARY quota but is still a
// request as far as the secondary (burst) limiter is concerned.
func githubTransportChain(inner http.RoundTripper) http.RoundTripper {
	return slowStartWrap(etagCacheWrap(inner))
}

// etagCacheKey identifies a cacheable representation: who asked, for what,
// in which format.
func etagCacheKey(req *http.Request) string {
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8]) + " " + req.Header.Get("Accept") + " " + req.URL.String()
}

// etagCacheable reports whether the request may be served conditionally: a
// plain GET with no conditional or range semantics of its own.
func etagCacheable(req *http.Request) bool {
	if req.Method != http.MethodGet {
		return false
	}
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "If-Match", "Range"} {
		if req.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func (c *etagCache) get(key string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	now := c.now()
	if now.Sub(e.lastUsed) > c.ttl {
		c.totalBytes -= len(e.body)
		delete(c.entries, key)
		return nil
	}
	e.lastUsed = now
	return e
}

func (c *etagCache) put(key string, e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.totalBytes -= len(old.body)
	}
	e.storedAt = c.now()
	e.lastUsed = e.storedAt
	c.entries[key] = e
	c.totalBytes += len(e.body)
	c.stores.Add(1)
	c.evictLocked()
}

func (c *etagCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.totalBytes -= len(old.body)
		delete(c.entries, key)
	}
}

// evictLocked removes least-recently-used entries until the cache is within
// both bounds. Callers hold c.mu.
func (c *etagCache) evictLocked() {
	for len(c.entries) > c.maxEntries || c.totalBytes > c.maxBytes {
		var oldestKey string
		var oldest *etagEntry
		for k, e := range c.entries {
			if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			return
		}
		c.totalBytes -= len(oldest.body)
		delete(c.entries, oldestKey)
	}
}

// replayHeaders are the per-response headers a replayed 200 must take from
// the 304 rather than from the remembered fetch: quota bookkeeping and dates.
var replayHeaders = []string{
	"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset",
	"X-RateLimit-Used", "X-RateLimit-Resource", "Date", "X-GitHub-Request-Id",
}

func (t *etagCacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !etagCacheable(req) {
		return t.inner.RoundTrip(req)
	}
	key := etagCacheKey(req)
	entry := t.cache.get(key)
	sent := req
	if entry != nil {
		sent = req.Clone(req.Context())
		sent.Header.Set("If-None-Match", entry.etag)
	}

	resp, err := t.inner.RoundTrip(sent)
	if err != nil || resp == nil {
		return resp, err
	}

	if entry != nil && resp.StatusCode == http.StatusNotModified {
		// Free round trip: hand back the remembered 200 with this response's
		// quota headers. Drain the 304 so the connection is reusable.
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, etagCacheMaxBody))
			_ = resp.Body.Close()
		}
		t.cache.hits.Add(1)
		hdr := entry.header.Clone()
		for _, h := range replayHeaders {
			if v := resp.Header.Get(h); v != "" {
				hdr.Set(h, v)
			}
		}
		hdr.Set(etagCacheHeader, "revalidated")
		hdr.Set("Content-Length", strconv.Itoa(len(entry.body)))
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         resp.Proto,
			ProtoMajor:    resp.ProtoMajor,
			ProtoMinor:    resp.ProtoMinor,
			Header:        hdr,
			Body:          io.NopCloser(bytes.NewReader(entry.body)),
			ContentLength: int64(len(entry.body)),
			Request:       req,
		}, nil
	}

	t.cache.misses.Add(1)
	switch {
	case resp.StatusCode == http.StatusOK:
		etag := resp.Header.Get("ETag")
		ct := resp.Header.Get("Content-Type")
		if etag == "" || !strings.Contains(ct, "json") || resp.Body == nil {
			return resp, nil
		}
		if resp.ContentLength > int64(t.cache.maxBody) {
			return resp, nil
		}
		// Buffer the body so it can be both remembered and returned. A body
		// that turns out larger than the bound is passed through uncached.
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(t.cache.maxBody)+1))
		_ = resp.Body.Close()
		if rerr != nil {
			resp.Body = io.NopCloser(bytes.NewReader(buf))
			return resp, rerr
		}
		resp.Body = io.NopCloser(bytes.NewReader(buf))
		if len(buf) > t.cache.maxBody {
			return resp, nil
		}
		t.cache.put(key, &etagEntry{etag: etag, header: resp.Header.Clone(), body: buf})
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden:
		// The resource is gone or no longer visible to this identity; the
		// remembered body must not be revalidated against it again. 403/429
		// are (usually) rate limits and say nothing about the resource.
		if entry != nil {
			t.cache.drop(key)
		}
	}
	return resp, nil
}

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
//
// Bodies of etagCacheCompressMin bytes or more are held gzip-compressed and
// the byte bound counts what is held. A closed-PR list page is ~2 MiB of
// JSON and the hive re-lists such pages for every watched repo in several
// query variants each cycle: on the projectbluefin hive (2026-11-08, 20
// repos) one variant alone was 36 MiB, so a raw 64 MiB bound let each sweep
// push the rest of the working set out, and a sweep that cycles through more
// bytes than the bound gets no 304 at all under least-recently-used eviction.
// GitHub JSON compresses about 7x at gzip's fastest level, so the same
// memory now holds the working set. HIVE_GITHUB_ETAG_CACHE_MAX_MB sets the
// bound for a hive that watches more than that.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
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
	// recently used entries are evicted past either. The byte bound counts
	// what is held: compressed bodies plus the remembered headers.
	// etagCacheMaxBytesEnv overrides the byte bound, in MiB.
	etagCacheMaxEntries  = 16384
	etagCacheMaxBytes    = 64 << 20
	etagCacheMaxBytesEnv = "HIVE_GITHUB_ETAG_CACHE_MAX_MB"
	// etagCacheCompressMin is the smallest body held compressed; below it
	// the gzip framing costs more than it saves.
	etagCacheCompressMin = 1 << 10
	// etagCacheTTL is how long an entry may be reused for revalidation. An
	// ETag never goes stale on GitHub's side, but an entry nobody has asked
	// about in this long is just memory.
	etagCacheTTL = 6 * time.Hour
	// etagCacheHeader marks a replayed response so a debugging operator can
	// tell a free revalidation from a charged fetch.
	etagCacheHeader = "X-Hive-Cache"
)

type etagEntry struct {
	etag   string
	header http.Header
	// body is what is held: the response body, gzip-compressed when gz is
	// set. rawLen is the body's length as GitHub sent it.
	body     []byte
	gz       bool
	rawLen   int
	size     int
	storedAt time.Time
	lastUsed time.Time
}

// newETagEntry remembers a 200's body and headers, compressing the body when
// that is worth it.
func newETagEntry(etag string, header http.Header, raw []byte) *etagEntry {
	e := &etagEntry{etag: etag, header: header, body: raw, rawLen: len(raw)}
	if len(raw) >= etagCacheCompressMin {
		var buf bytes.Buffer
		buf.Grow(len(raw) / 4)
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		if err == nil {
			_, err = zw.Write(raw)
			if cerr := zw.Close(); err == nil {
				err = cerr
			}
		}
		if err == nil && buf.Len() < len(raw) {
			e.body, e.gz = bytes.Clone(buf.Bytes()), true
		}
	}
	e.size = len(e.body)
	for k, vs := range header {
		e.size += len(k)
		for _, v := range vs {
			e.size += len(v)
		}
	}
	return e
}

// rawBody returns the body as GitHub sent it.
func (e *etagEntry) rawBody() ([]byte, error) {
	if !e.gz {
		return e.body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(e.body))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, e.rawLen)
	buf := bytes.NewBuffer(out)
	if _, err := io.Copy(buf, io.LimitReader(zr, int64(e.rawLen)+1)); err != nil {
		return nil, err
	}
	if buf.Len() != e.rawLen {
		return nil, io.ErrUnexpectedEOF
	}
	return buf.Bytes(), nil
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
	// did not charge; an eviction is an entry pushed out by a bound before
	// anyone asked for it again.
	hits, misses, stores, evictions atomic.Int64
}

func newETagCache() *etagCache {
	return &etagCache{
		entries:    map[string]*etagEntry{},
		maxEntries: etagCacheMaxEntries,
		maxBytes:   etagCacheMaxBytesFromEnv(),
		maxBody:    etagCacheMaxBody,
		ttl:        etagCacheTTL,
		now:        time.Now,
	}
}

// etagCacheMaxBytesFromEnv is the byte bound: HIVE_GITHUB_ETAG_CACHE_MAX_MB
// MiB when that is a positive integer, etagCacheMaxBytes otherwise.
func etagCacheMaxBytesFromEnv() int {
	raw := strings.TrimSpace(os.Getenv(etagCacheMaxBytesEnv))
	if raw == "" {
		return etagCacheMaxBytes
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb <= 0 || mb > 1<<20 {
		return etagCacheMaxBytes
	}
	return mb << 20
}

// sharedETagCache is the store every wrapped client shares.
var sharedETagCache = newETagCache()

// ETagCacheStats reports how many requests were answered by a free 304
// revalidation, how many were charged fetches, and how many bodies are held.
func ETagCacheStats() (hits, misses, entries int64) {
	info := ETagCacheSnapshot()
	return info.Hits, info.Misses, info.Entries
}

// ETagCacheSnapshot is ETagCacheStats plus how many bytes are held against
// which bound, and how many entries a bound has pushed out. A rising
// evictions count with a byte count at the bound means the working set does
// not fit (HIVE_GITHUB_ETAG_CACHE_MAX_MB).
func ETagCacheSnapshot() ETagCacheInfo {
	c := sharedETagCache
	c.mu.Lock()
	n, held, limit := len(c.entries), c.totalBytes, c.maxBytes
	c.mu.Unlock()
	return ETagCacheInfo{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Entries:   int64(n),
		Bytes:     int64(held),
		MaxBytes:  int64(limit),
		Evictions: c.evictions.Load(),
	}
}

type etagCacheTransport struct {
	inner http.RoundTripper
	cache *etagCache
}

func etagCacheWrap(inner http.RoundTripper) http.RoundTripper {
	return &etagCacheTransport{inner: inner, cache: sharedETagCache}
}

// githubTransportChain is the set of process-wide behaviours every GitHub
// client's requests pass through, in order: the OTel span/metrics layer
// (otel_transport.go) outermost so one span covers the whole call, then
// pacing after a rate limit (slowstart.go), then conditional-request caching,
// then the shared socket-owning transport. Pacing sits outside the cache on
// purpose: a conditional request is free against the PRIMARY quota but is
// still a request as far as the secondary (burst) limiter is concerned.
func githubTransportChain(inner http.RoundTripper) http.RoundTripper {
	return otelWrap(slowStartWrap(githubUnpacedTransportChain(inner)))
}

// githubUnpacedTransportChain is githubTransportChain without the slow-start
// pacer. It is for the App-JWT endpoints (installation-token mint, app
// discovery): they are a separate rate-limit bucket from the installation's
// REST quota, and a mint that queues behind paced traffic fails every
// request that needed the token — observed live 2026-10-21 ("creating
// installation token: ... slot would miss request deadline").
func githubUnpacedTransportChain(inner http.RoundTripper) http.RoundTripper {
	return etagCacheWrap(rest404NegativeCacheWrap(restAccountingWrap(inner)))
}

// githubAppJWTTransportChain is githubUnpacedTransportChain for requests
// authenticated with the App JWT. Their rate-limit headers describe the App's
// JWT bucket, so REST accounting counts them per caller but keeps them out of
// the installation's core snapshot that drives the API budget mode.
func githubAppJWTTransportChain(inner http.RoundTripper) http.RoundTripper {
	return etagCacheWrap(rest404NegativeCacheWrap(restAccountingWrapAppJWT(inner)))
}

// externalSlowStartState paces requests made through NewHTTPTransport. It is
// deliberately separate from sharedSlowStartState: NewHTTPTransport callers
// (the hub's pollers) may run anonymously, and a per-IP anonymous 403 from
// api.github.com must not throttle the authenticated go-github clients that
// share the process — the cmd/hive test binary boots both.
var externalSlowStartState = newSlowStartState()

// NewHTTPTransport returns the shared GitHub REST transport chain wrapped
// around base: ETag/304 replay, 404 negative caching and REST accounting are
// shared with every other GitHub client in the process; rate-limit pacing
// state is private to this chain. Passing nil uses hive's proxy-trusting
// shared socket transport.
func NewHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = sharedProxyTrust.sharedTransport()
	}
	return otelWrap(&slowStartTransport{
		inner: etagCacheWrap(rest404NegativeCacheWrap(restAccountingWrap(base))),
		state: externalSlowStartState,
	})
}

// etagCacheKey identifies a cacheable representation: who asked, for what,
// in which format.
func etagCacheKey(req *http.Request) string {
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8]) + " " + req.Header.Get("Accept") + " " + req.URL.String()
}

// cacheableSinceGranularity is how far down a list query's since is rounded.
const cacheableSinceGranularity = time.Hour

// cacheableSince rounds the since of a GitHub list query down to the hour (in
// UTC), so every pass inside that hour asks for the same URL.
//
// The ETag cache above is keyed on the full URL, query included. A sweep that
// builds its since from time.Now() on every pass ("comments updated in the
// last 7 days") changes the URL on every call, so the cache never matches and
// every pass is charged in full. In 50 minutes on the knuckle hive
// (2026-11-07) the hive's own issue-comment lists were charged 1357 times in
// 2776 and its issue lists 1012 times in 1399, while its pull-request lists
// and review reads came back as free 304s 83% and 91% of the time.
//
// Rounding only ever moves since earlier, by less than an hour, so the list
// is a superset of what was asked for and nothing is missed. Callers keep
// filtering on their exact cutoff. A zero since (no filter) stays zero.
func cacheableSince(since time.Time) time.Time {
	if since.IsZero() {
		return since
	}
	return since.UTC().Truncate(cacheableSinceGranularity)
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
		c.totalBytes -= e.size
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
		c.totalBytes -= old.size
	}
	e.storedAt = c.now()
	e.lastUsed = e.storedAt
	c.entries[key] = e
	c.totalBytes += e.size
	c.stores.Add(1)
	c.evictLocked()
}

func (c *etagCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.totalBytes -= old.size
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
		c.totalBytes -= oldest.size
		delete(c.entries, oldestKey)
		c.evictions.Add(1)
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
		body, berr := entry.rawBody()
		if berr != nil {
			// The held copy cannot be read back: forget it and fetch the
			// resource plainly rather than hand the caller a broken body.
			t.cache.drop(key)
			t.cache.misses.Add(1)
			return t.inner.RoundTrip(req)
		}
		t.cache.hits.Add(1)
		hdr := entry.header.Clone()
		for _, h := range replayHeaders {
			if v := resp.Header.Get(h); v != "" {
				hdr.Set(h, v)
			}
		}
		hdr.Set(etagCacheHeader, "revalidated")
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         resp.Proto,
			ProtoMajor:    resp.ProtoMajor,
			ProtoMinor:    resp.ProtoMinor,
			Header:        hdr,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
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
		t.cache.put(key, newETagEntry(etag, resp.Header.Clone(), buf))
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

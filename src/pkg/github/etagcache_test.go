package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// These tests pin #7430: repeat GETs are conditional, a 304 is replayed as the
// remembered 200 (GitHub does not charge a 304 against the REST quota), and
// the primary rate limit engages the same post-reset pacing the secondary
// limit already had.

// etagServer answers a JSON list with an ETag, and 304 when the client
// presents that ETag. It records every request it sees.
type etagServer struct {
	mu   sync.Mutex
	seen []*http.Request
	body string
	etag string
	// remaining is echoed as X-RateLimit-Remaining, decremented per charged
	// request, so a test can assert what GitHub would have billed.
	remaining int
}

func (s *etagServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, r.Clone(context.Background()))
	w.Header().Set("X-RateLimit-Limit", "6650")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if r.Header.Get("If-None-Match") == s.etag {
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(s.remaining))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.remaining--
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(s.remaining))
	w.Header().Set("ETag", s.etag)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Link", `<`+"http://"+r.Host+r.URL.Path+`?page=2>; rel="next"`)
	_, _ = io.WriteString(w, s.body)
}

func newETagTransport() (*etagCacheTransport, *etagCache) {
	c := newETagCache()
	return &etagCacheTransport{inner: http.DefaultTransport, cache: c}, c
}

func etagGet(t *testing.T, tr http.RoundTripper, url, auth string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestETagCache_RevalidatesForFree(t *testing.T) {
	srv := &etagServer{body: `[{"number":1,"title":"open issue"}]`, etag: `W/"abc123"`, remaining: 100}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	tr, cache := newETagTransport()
	url := ts.URL + "/repos/projectbluefin/chairlift/issues?per_page=100&state=open"

	// First fetch: charged, remembered.
	resp, body := etagGet(t, tr, url, "tok-app")
	if resp.StatusCode != http.StatusOK || body != srv.body {
		t.Fatalf("first fetch = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get(etagCacheHeader) != "" {
		t.Error("a charged fetch must not be marked as revalidated")
	}

	// Second fetch: conditional, answered 304, replayed as the remembered 200.
	resp, body = etagGet(t, tr, url, "tok-app")
	if resp.StatusCode != http.StatusOK || body != srv.body {
		t.Fatalf("revalidated fetch = %d %q, want the remembered 200 body", resp.StatusCode, body)
	}
	if resp.Header.Get(etagCacheHeader) != "revalidated" {
		t.Error("replayed response is not marked revalidated")
	}
	if got := resp.Header.Get("Link"); got == "" {
		t.Error("replayed response lost the Link header go-github paginates on")
	}
	// Quota headers come from the 304, not the stale 200.
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "99" {
		t.Errorf("replayed X-RateLimit-Remaining = %q, want the 304's 99 (the 200's said 99 too, but the server must not have charged again)", got)
	}
	if resp.ContentLength != int64(len(srv.body)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(srv.body)) {
		t.Errorf("replayed length = %d / %q, want %d", resp.ContentLength, resp.Header.Get("Content-Length"), len(srv.body))
	}

	srv.mu.Lock()
	seen := len(srv.seen)
	inm := srv.seen[1].Header.Get("If-None-Match")
	remaining := srv.remaining
	srv.mu.Unlock()
	if seen != 2 {
		t.Fatalf("server saw %d requests, want 2", seen)
	}
	if inm != srv.etag {
		t.Errorf("second request If-None-Match = %q, want %q", inm, srv.etag)
	}
	if remaining != 99 {
		t.Errorf("server charged %d requests, want exactly 1 — the 304 must be free (#7430)", 100-remaining)
	}
	if hits, misses, entries := cache.hits.Load(), cache.misses.Load(), len(cache.entries); hits != 1 || misses != 1 || entries != 1 {
		t.Errorf("hits/misses/entries = %d/%d/%d, want 1/1/1", hits, misses, entries)
	}
}

func TestETagCache_KeyedByIdentityAndAccept(t *testing.T) {
	srv := &etagServer{body: `[]`, etag: `"e1"`, remaining: 100}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	tr, _ := newETagTransport()
	url := ts.URL + "/repos/o/r/pulls?state=open"

	etagGet(t, tr, url, "tok-app")
	etagGet(t, tr, url, "tok-agent-scoped")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(srv.seen))
	}
	if srv.seen[1].Header.Get("If-None-Match") != "" {
		t.Error("a different identity was handed the App's ETag: a scoped token must miss, not revalidate against another identity's body")
	}
	if srv.remaining != 98 {
		t.Errorf("charged %d, want 2 (one per identity)", 100-srv.remaining)
	}
}

func TestETagCache_PassThroughAndNoCache(t *testing.T) {
	srv := &etagServer{body: `{"ok":true}`, etag: `"e1"`, remaining: 100}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	tr, cache := newETagTransport()

	// A caller's own conditional request is never rewritten or cached.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/x", nil)
	req.Header.Set("If-None-Match", `"someone-elses"`)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(cache.entries) != 0 {
		t.Error("a request with its own conditional header was cached")
	}

	// A mutation is never cached and never made conditional.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/x", nil)
	resp, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(cache.entries) != 0 {
		t.Error("a POST response was cached")
	}

	// A non-JSON or ETag-less 200 is passed through uncached.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"bin"`)
		_, _ = io.WriteString(w, "tarball")
	}))
	defer plain.Close()
	resp, body := etagGet(t, tr, plain.URL+"/tarball", "")
	if resp.StatusCode != 200 || body != "tarball" || len(cache.entries) != 0 {
		t.Errorf("non-JSON response: %d %q entries=%d", resp.StatusCode, body, len(cache.entries))
	}
}

func TestETagCache_DropsEntryWhenResourceGoes(t *testing.T) {
	var gone bool
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		g := gone
		mu.Unlock()
		if g {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"e1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"n":1}`)
	}))
	defer ts.Close()
	tr, cache := newETagTransport()
	etagGet(t, tr, ts.URL+"/repos/o/r", "t")
	if len(cache.entries) != 1 {
		t.Fatal("first fetch not cached")
	}
	mu.Lock()
	gone = true
	mu.Unlock()
	resp, _ := etagGet(t, tr, ts.URL+"/repos/o/r", "t")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status after deletion = %d, want 404 passed through", resp.StatusCode)
	}
	if len(cache.entries) != 0 {
		t.Error("a 404 left the stale entry in place; it would be revalidated forever")
	}
}

func TestETagCache_EvictsLeastRecentlyUsed(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("ETag", fmt.Sprintf(`"e%d"`, n))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ts.Close()
	tr, cache := newETagTransport()
	cache.maxEntries = 3
	for i := 0; i < 5; i++ {
		etagGet(t, tr, fmt.Sprintf("%s/r/%d", ts.URL, i), "t")
	}
	if len(cache.entries) != 3 {
		t.Fatalf("entries = %d, want bounded at 3", len(cache.entries))
	}
	for i := 0; i < 2; i++ {
		if cache.get(etagCacheKeyFor(t, fmt.Sprintf("%s/r/%d", ts.URL, i), "t")) != nil {
			t.Errorf("oldest entry %d survived eviction", i)
		}
	}
	if cache.totalBytes != 3*len(`{}`) {
		t.Errorf("totalBytes = %d, want %d", cache.totalBytes, 3*len(`{}`))
	}
}

// resetSharedSlowStart clears the process-wide pacing ledger so a test that
// deliberately provokes a rate-limit 403 through a real client does not pace
// every later test in the package.
func resetSharedSlowStart() { ResetRateLimitPacingForTest() }

// socketTransportOf walks the githubTransportChain wrappers (slow-start
// pacer, ETag cache) down to the socket-owning *http.Transport.
func socketTransportOf(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	for {
		switch w := rt.(type) {
		case *slowStartTransport:
			rt = w.inner
		case *etagCacheTransport:
			rt = w.inner
		case *http.Transport:
			return w
		default:
			t.Fatalf("unexpected transport layer %T", rt)
		}
	}
}

func etagCacheKeyFor(t *testing.T, url, auth string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+auth)
	return etagCacheKey(req)
}

// TestETagCache_ThroughGoGithub: the replayed response is a real 200 as far
// as go-github is concerned — the second Issues.ListByRepo decodes the
// remembered body and still sees the pagination Link.
func TestETagCache_ThroughGoGithub(t *testing.T) {
	srv := &etagServer{body: `[{"number":7,"title":"t"}]`, etag: `W/"list"`, remaining: 10}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()
	tr, cache := newETagTransport()
	client := gh.NewClient(&http.Client{Transport: tr}).WithAuthToken("tok")
	setBaseURL(client, ts.URL+"/")

	for i := 0; i < 2; i++ {
		issues, resp, err := client.Issues.ListByRepo(context.Background(), "o", "r", &gh.IssueListByRepoOptions{State: "open"})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(issues) != 1 || issues[0].GetNumber() != 7 {
			t.Fatalf("call %d decoded %+v", i, issues)
		}
		if resp.NextPage != 2 {
			t.Errorf("call %d NextPage = %d, want 2 from the Link header", i, resp.NextPage)
		}
	}
	if cache.hits.Load() != 1 {
		t.Errorf("hits = %d, want the second call revalidated", cache.hits.Load())
	}
	if srv.remaining != 9 {
		t.Errorf("GitHub would have charged %d, want 1", 10-srv.remaining)
	}
}

// TestSlowStart_PrimaryLimitEngagesCaution: a primary-limit 403 (Remaining 0
// with a reset time) must engage the caution window from the RESET, so the
// post-reset wave is paced — the projectbluefin spoke re-exhausted a 6650/hr
// quota within one second of every reset (#7430).
func TestSlowStart_PrimaryLimitEngagesCaution(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "6650")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 135058248."}`)
	}))
	defer ts.Close()
	tr := newSlowStartTransport(http.DefaultTransport)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/repos/o/r/issues", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(body) == 0 {
		t.Fatalf("the 403 and its body must pass through untouched: %d %q", resp.StatusCode, body)
	}
	tr.state.mu.Lock()
	until := tr.state.cautiousUntil
	tr.state.mu.Unlock()
	want := reset.Add(tr.state.window)
	if !until.Equal(want) {
		t.Fatalf("cautiousUntil = %v, want reset+window = %v", until, want)
	}

	// A reset absurdly far ahead is capped at an hour: pacing until 2286
	// would idle the hive on one bad header.
	far := &http.Response{Header: http.Header{}}
	far.Header.Set("X-RateLimit-Remaining", "0")
	far.Header.Set("X-RateLimit-Reset", "9999999999")
	now := time.Now()
	capped, ok := primaryLimitReset(far, now)
	if !ok || !capped.Equal(now.Add(primaryLimitMaxReset)) {
		t.Errorf("far-future reset = %v (%v), want capped at now+%v", capped, ok, primaryLimitMaxReset)
	}

	// A permission 403 (Remaining > 0) engages nothing.
	tr2 := newSlowStartTransport(http.DefaultTransport)
	perm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer perm.Close()
	req, _ = http.NewRequest(http.MethodGet, perm.URL+"/x", nil)
	resp, _ = tr2.RoundTrip(req)
	_ = resp.Body.Close()
	tr2.state.mu.Lock()
	defer tr2.state.mu.Unlock()
	if !tr2.state.cautiousUntil.IsZero() {
		t.Error("a permission 403 engaged rate-limit caution")
	}
}

// TestGitHubTransportChain_AppClientIsPacedAndCached: the App transport used
// to hit the raw shared transport, outside both behaviours. It must now use
// the same chain as every other client.
func TestGitHubTransportChain_AppClientIsPacedAndCached(t *testing.T) {
	chain := githubTransportChain(http.DefaultTransport)
	ss, ok := chain.(*slowStartTransport)
	if !ok {
		t.Fatalf("chain outermost = %T, want *slowStartTransport", chain)
	}
	if ss.state != sharedSlowStartState {
		t.Error("chain does not use the shared pacing ledger")
	}
	ec, ok := ss.inner.(*etagCacheTransport)
	if !ok {
		t.Fatalf("chain inner = %T, want *etagCacheTransport", ss.inner)
	}
	if ec.cache != sharedETagCache || ec.inner != http.DefaultTransport {
		t.Error("chain does not use the shared ETag cache over the given transport")
	}
}

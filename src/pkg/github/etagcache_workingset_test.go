package github

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// prListBody builds a JSON page shaped like GET /repos/{o}/{r}/pulls with
// state=closed&per_page=100: about 2 MiB, which is what a busy projectbluefin
// repo returns (projectbluefin/common measured 2,124,219 bytes on 2026-11-08).
// Every object carries random SHAs and node IDs so the page does not
// compress better than the real one does.
func prListBody(t *testing.T, repo string) []byte {
	t.Helper()
	randHex := func(n int) string {
		b := make([]byte, n/2)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := 0; buf.Len() < 2<<20; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		base := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repo, i)
		fmt.Fprintf(&buf, `{"url":%q,"id":%d,"node_id":"PR_%s","number":%d,"state":"closed","title":"fix(%s): change %s",`+
			`"body":%q,"user":{"login":"user-%s","id":%d,"node_id":"U_%s","avatar_url":"https://avatars.githubusercontent.com/u/%d?v=4"},`+
			`"merge_commit_sha":%q,"head":{"ref":"fix/%s","sha":%q,"repo":{"full_name":%q,"node_id":"R_%s"}},`+
			`"base":{"ref":"main","sha":%q,"repo":{"full_name":%q,"node_id":"R_%s"}},`+
			`"labels":[{"id":%d,"node_id":"LA_%s","name":"kind/bug","color":"d73a4a"}],`+
			`"_links":{"self":{"href":%q},"html":{"href":"https://github.com/%s/pull/%d"},"commits":{"href":"%s/commits"},"comments":{"href":"%s/comments"}},`+
			`"created_at":"2026-11-0%dT0%d:%02d:00Z","updated_at":"2026-11-0%dT1%d:%02d:00Z","closed_at":"2026-11-07T12:00:00Z","merged_at":"2026-11-07T12:00:00Z"}`,
			base, 2_900_000_000+i, randHex(16), i, repo, randHex(8),
			"Refs #"+fmt.Sprint(i)+". "+randHex(64)+" "+strings.Repeat("Signed-off-by: someone <someone@example.com> ", 2),
			randHex(6), 100_000+i, randHex(12), 100_000+i,
			randHex(40), randHex(8), randHex(40), repo, randHex(12),
			randHex(40), repo, randHex(12),
			8_000_000_000+i, randHex(16),
			base, repo, i, base, base,
			1+i%7, i%10, i%60, 1+i%7, i%10, i%60)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// prListOrigin plays GitHub for a fixed set of list URLs: 200 with an ETag on
// a plain GET, 304 when the request presents that ETag. It counts the charged
// (200) answers.
type prListOrigin struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	charged int
}

func (o *prListOrigin) RoundTrip(req *http.Request) (*http.Response, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	body, ok := o.bodies[req.URL.Path]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	}
	etag := `W/"` + req.URL.Path + `"`
	if req.Header.Get("If-None-Match") == etag {
		return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{"Etag": {etag}}, Body: http.NoBody, Request: req}, nil
	}
	o.charged++
	h := http.Header{}
	h.Set("ETag", etag)
	h.Set("Content-Type", "application/json; charset=utf-8")
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
}

// The live projectbluefin hive (2026-11-08) re-lists ~2 MiB closed-PR pages
// for 20 repos in several query variants every cycle. The cache used to hold
// 64 MiB of raw bodies, so one sweep of those lists pushed the rest of the
// working set out, and a sweep that cycles through more bytes than the bound
// never got a single free 304 from least-recently-used eviction. A working
// set larger than the old raw bound must keep every revalidation free.
func TestETagCache_WorkingSetLargerThanOldBoundKeepsHits(t *testing.T) {
	const repos = 40 // 40 x ~2 MiB = ~80 MiB of raw JSON, past the old 64 MiB bound
	origin := &prListOrigin{bodies: map[string][]byte{}}
	var raw int
	for i := 0; i < repos; i++ {
		repo := fmt.Sprintf("projectbluefin/repo-%02d", i)
		b := prListBody(t, repo)
		origin.bodies["/repos/"+repo+"/pulls"] = b
		raw += len(b)
	}
	if raw <= 64<<20 {
		t.Fatalf("fixture working set = %d bytes, want more than the old 64 MiB bound", raw)
	}
	// The fixture must not compress better than real GitHub JSON does
	// (projectbluefin/common's closed-PR page: 2,124,219 -> 298,684 bytes
	// at gzip -1, 7.1x), or the test would prove less than it claims.
	sample := origin.bodies["/repos/projectbluefin/repo-00/pulls"]
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestSpeed)
	_, _ = zw.Write(sample)
	_ = zw.Close()
	ratio := float64(len(sample)) / float64(gz.Len())
	t.Logf("fixture: %d bytes raw over %d lists, gzip -1 ratio %.1fx", raw, repos, ratio)
	if ratio > 7.1 {
		t.Fatalf("fixture compresses %.1fx, more than the 7.1x real GitHub pages do", ratio)
	}

	cache := newETagCache()
	tr := &etagCacheTransport{inner: origin, cache: cache}
	get := func(repo string) string {
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/pulls?direction=desc&per_page=100&sort=updated&state=closed", nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer app-token")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s: %v", repo, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", repo, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Length"); resp.Header.Get(etagCacheHeader) != "" && got != fmt.Sprint(len(body)) {
			t.Fatalf("GET %s Content-Length = %s, body is %d bytes", repo, got, len(body))
		}
		return string(body)
	}

	for pass := 0; pass < 3; pass++ {
		for i := 0; i < repos; i++ {
			repo := fmt.Sprintf("projectbluefin/repo-%02d", i)
			if got := get(repo); got != string(origin.bodies["/repos/"+repo+"/pulls"]) {
				t.Fatalf("pass %d: %s body differs from what GitHub sent", pass, repo)
			}
		}
	}
	if origin.charged != repos {
		t.Fatalf("charged fetches = %d over 3 sweeps of %d unchanged lists, want %d (only the first sweep); hits=%d misses=%d entries=%d",
			origin.charged, repos, repos, cache.hits.Load(), cache.misses.Load(), len(cache.entries))
	}
	if got := cache.hits.Load(); got != 2*repos {
		t.Fatalf("hits = %d, want %d", got, 2*repos)
	}
}

// A held body that cannot be read back is never handed to the caller: the
// entry is dropped and the resource fetched plainly.
func TestETagCache_UnreadableHeldBodyFetchesPlainly(t *testing.T) {
	origin := &prListOrigin{bodies: map[string][]byte{"/repos/o/r/pulls": prListBody(t, "o/r")}}
	cache := newETagCache()
	tr := &etagCacheTransport{inner: origin, cache: cache}
	url := "https://api.github.com/repos/o/r/pulls?state=closed"
	get := func() []byte {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer app-token")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return b
	}
	get()
	cache.mu.Lock()
	if len(cache.entries) != 1 {
		cache.mu.Unlock()
		t.Fatalf("entries = %d, want 1", len(cache.entries))
	}
	for _, e := range cache.entries {
		if !e.gzipped || len(e.body) >= e.size {
			t.Errorf("a %d-byte JSON body is held as %d bytes (gzipped=%v), want it compressed", e.size, len(e.body), e.gzipped)
		}
		e.body = []byte("not gzip")
	}
	cache.mu.Unlock()

	if got := get(); !bytes.Equal(got, origin.bodies["/repos/o/r/pulls"]) {
		t.Fatalf("body after an unreadable held copy differs from GitHub's (%d bytes)", len(got))
	}
	if origin.charged != 2 {
		t.Fatalf("charged = %d, want 2 (the plain re-fetch)", origin.charged)
	}
	if cache.hits.Load() != 0 {
		t.Fatalf("hits = %d, want 0", cache.hits.Load())
	}
}

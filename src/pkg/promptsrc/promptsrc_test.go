package promptsrc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// stubFetcher is a test Fetcher. It records call counts and returns a scripted
// body/error, letting us drive the cache/fallback paths deterministically.
type stubFetcher struct {
	mu    sync.Mutex
	body  string
	err   error
	calls int
	// lastRef captures the ref passed on the most recent call.
	lastRef string
}

func (f *stubFetcher) GetFileContentRef(_ context.Context, _, _, _, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastRef = ref
	return f.body, f.err
}

func (f *stubFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func allowAll(string) bool  { return true }
func allowNone(string) bool { return false }

func fullSource() Source {
	return Source{Owner: "hivecommons", Repo: "hive", Path: "prompts/scanner.md", Ref: "v2"}
}

// TestResolve_HappyPath: a valid, allowlisted source is fetched and returned.
func TestResolve_HappyPath(t *testing.T) {
	f := &stubFetcher{body: "you are the scanner"}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), fullSource())
	if !res.Ok || res.Body != "you are the scanner" {
		t.Fatalf("want ok body, got ok=%v body=%q", res.Ok, res.Body)
	}
	if !strings.HasPrefix(res.Source, "github:hivecommons/hive@v2") {
		t.Errorf("provenance = %q, want github:hivecommons/hive@v2", res.Source)
	}
	if f.lastRef != "v2" {
		t.Errorf("ref not passed through: %q", f.lastRef)
	}
}

// TestResolve_UnsetSource: an incomplete source is a miss (Ok=false), never a fetch.
func TestResolve_UnsetSource(t *testing.T) {
	f := &stubFetcher{body: "x"}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), Source{Owner: "o", Repo: "r"}) // no path
	if res.Ok {
		t.Errorf("incomplete source should be a miss, got %+v", res)
	}
	if f.callCount() != 0 {
		t.Errorf("incomplete source should not fetch, calls=%d", f.callCount())
	}
	if res.Source != "unset" {
		t.Errorf("source = %q, want unset", res.Source)
	}
}

// TestResolve_DeniedByAllowlist: a repo not on the allowlist is never fetched.
func TestResolve_DeniedByAllowlist(t *testing.T) {
	f := &stubFetcher{body: "secret"}
	r := NewResolver(f, allowNone, nil)
	res := r.Resolve(context.Background(), fullSource())
	if res.Ok {
		t.Errorf("denied source should be a miss, got %+v", res)
	}
	if f.callCount() != 0 {
		t.Errorf("denied source must not fetch (SSRF/exfil guard), calls=%d", f.callCount())
	}
	if res.Source != "denied" {
		t.Errorf("source = %q, want denied", res.Source)
	}
}

// TestResolve_NilAllowDeniesAll: a nil AllowFunc fails closed.
func TestResolve_NilAllowDeniesAll(t *testing.T) {
	f := &stubFetcher{body: "secret"}
	r := NewResolver(f, nil, nil)
	res := r.Resolve(context.Background(), fullSource())
	if res.Ok || f.callCount() != 0 {
		t.Errorf("nil allow must deny all, got ok=%v calls=%d", res.Ok, f.callCount())
	}
}

// TestResolve_FallbackToCacheOnError: after one success, a later failure serves
// the last-known-good body rather than blanking the kick.
func TestResolve_FallbackToCacheOnError(t *testing.T) {
	f := &stubFetcher{body: "good prompt"}
	r := NewResolver(f, allowAll, nil)

	first := r.Resolve(context.Background(), fullSource())
	if !first.Ok || first.Body != "good prompt" {
		t.Fatalf("first resolve should succeed, got %+v", first)
	}

	// Now the repo becomes unreachable.
	f.mu.Lock()
	f.body = ""
	f.err = errors.New("boom: repo unreachable")
	f.mu.Unlock()

	second := r.Resolve(context.Background(), fullSource())
	if !second.Ok || second.Body != "good prompt" {
		t.Fatalf("failure should fall back to cache, got %+v", second)
	}
	if !strings.HasPrefix(second.Source, "github-cache:") {
		t.Errorf("provenance = %q, want github-cache:*", second.Source)
	}
}

// TestResolve_ErrorNoCache: a failure with no prior success is a miss so the
// caller falls back to its inline template — never an error, never a crash.
func TestResolve_ErrorNoCache(t *testing.T) {
	f := &stubFetcher{err: errors.New("boom")}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), fullSource())
	if res.Ok {
		t.Errorf("cold failure should be a miss, got %+v", res)
	}
	if res.Source != "error" {
		t.Errorf("source = %q, want error", res.Source)
	}
}

// TestResolve_NilFetcher: no client wired -> miss (caller uses inline template).
func TestResolve_NilFetcher(t *testing.T) {
	r := NewResolver(nil, allowAll, nil)
	res := r.Resolve(context.Background(), fullSource())
	if res.Ok || res.Source != "no-client" {
		t.Errorf("nil fetcher should miss with no-client, got %+v", res)
	}
}

// TestResolve_TruncatesOversizeBody: a body over the cap is truncated, not rejected.
func TestResolve_TruncatesOversizeBody(t *testing.T) {
	huge := strings.Repeat("a", maxPromptBytes+100)
	f := &stubFetcher{body: huge}
	r := NewResolver(f, allowAll, nil)
	res := r.Resolve(context.Background(), fullSource())
	if !res.Ok {
		t.Fatalf("oversize body should still resolve, got %+v", res)
	}
	if len(res.Body) != maxPromptBytes {
		t.Errorf("body len = %d, want %d (truncated to cap)", len(res.Body), maxPromptBytes)
	}
}

// TestResolve_NilContext: a nil context must not panic (kick callers pass Background,
// but defense-in-depth).
func TestResolve_NilContext(t *testing.T) {
	f := &stubFetcher{body: "ok"}
	r := NewResolver(f, allowAll, nil)
	//nolint:staticcheck // deliberately passing nil ctx to exercise the guard
	res := r.Resolve(nil, fullSource())
	if !res.Ok {
		t.Errorf("nil ctx should be tolerated, got %+v", res)
	}
}

// TestFetchOnce_Gating: FetchOnce enforces the same allowlist gate and surfaces
// errors (unlike Resolve) so the create/edit UI can report a bad source.
func TestFetchOnce_Gating(t *testing.T) {
	f := &stubFetcher{body: "baked"}

	if _, err := FetchOnce(context.Background(), f, allowNone, fullSource()); err == nil {
		t.Error("denied repo should error")
	}
	if f.callCount() != 0 {
		t.Errorf("denied FetchOnce must not fetch, calls=%d", f.callCount())
	}

	if _, err := FetchOnce(context.Background(), f, allowAll, Source{Owner: "o"}); err == nil {
		t.Error("incomplete source should error")
	}

	if _, err := FetchOnce(context.Background(), nil, allowAll, fullSource()); err == nil {
		t.Error("nil fetcher should error")
	}

	body, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err != nil || body != "baked" {
		t.Errorf("valid FetchOnce failed: body=%q err=%v", body, err)
	}
}

// TestResolve_NilFetcherServesCache: when the fetcher is nil but a prior
// success left a cached body (e.g. the client was swapped out after a
// successful boot), Resolve should still serve the cache instead of missing.
func TestResolve_NilFetcherServesCache(t *testing.T) {
	f := &stubFetcher{body: "cached prompt"}
	r := NewResolver(f, allowAll, nil)
	first := r.Resolve(context.Background(), fullSource())
	if !first.Ok {
		t.Fatalf("priming fetch should succeed, got %+v", first)
	}

	// Simulate the client going away (e.g. token-less rewire) while the cache
	// entry from the earlier successful fetch remains.
	r.fetcher = nil

	res := r.Resolve(context.Background(), fullSource())
	if !res.Ok || res.Body != "cached prompt" {
		t.Fatalf("nil fetcher with warm cache should hit, got %+v", res)
	}
	if !strings.HasPrefix(res.Source, "github-cache:") {
		t.Errorf("source = %q, want github-cache:*", res.Source)
	}
}

// TestResolve_DefaultRefProvenance: an empty Ref reports "default" in the
// provenance string rather than an empty ref segment.
func TestResolve_DefaultRefProvenance(t *testing.T) {
	f := &stubFetcher{body: "on default branch"}
	r := NewResolver(f, allowAll, nil)
	src := Source{Owner: "hivecommons", Repo: "hive", Path: "prompts/scanner.md"} // Ref unset
	res := r.Resolve(context.Background(), src)
	if !res.Ok {
		t.Fatalf("resolve should succeed, got %+v", res)
	}
	if res.Source != "github:hivecommons/hive@default" {
		t.Errorf("source = %q, want github:hivecommons/hive@default", res.Source)
	}
}

// TestFetchOnce_NilContext: a nil context must not panic and should behave
// like context.Background().
func TestFetchOnce_NilContext(t *testing.T) {
	f := &stubFetcher{body: "baked"}
	//nolint:staticcheck // deliberately passing nil ctx to exercise the guard
	body, err := FetchOnce(nil, f, allowAll, fullSource())
	if err != nil || body != "baked" {
		t.Errorf("nil ctx should be tolerated, got body=%q err=%v", body, err)
	}
}

// TestFetchOnce_TruncatesOversizeBody: FetchOnce enforces the same size cap
// as Resolve, truncating rather than erroring.
func TestFetchOnce_TruncatesOversizeBody(t *testing.T) {
	huge := strings.Repeat("b", maxPromptBytes+50)
	f := &stubFetcher{body: huge}
	body, err := FetchOnce(context.Background(), f, allowAll, fullSource())
	if err != nil {
		t.Fatalf("oversize body should not error, got %v", err)
	}
	if len(body) != maxPromptBytes {
		t.Errorf("body len = %d, want %d (truncated to cap)", len(body), maxPromptBytes)
	}
}

// TestSource_Helpers exercises Slug/valid/key edge cases.
func TestSource_Helpers(t *testing.T) {
	if (Source{}).Slug() != "" {
		t.Error("empty source slug should be empty")
	}
	if s := (Source{Owner: "o", Repo: "r"}); s.Slug() != "o/r" {
		t.Errorf("slug = %q", s.Slug())
	}
	// Two sources differing only by path must have distinct cache keys.
	a := Source{Owner: "o", Repo: "r", Path: "a.md"}
	b := Source{Owner: "o", Repo: "r", Path: "b.md"}
	if a.key() == b.key() {
		t.Error("different paths must not share a cache key")
	}
}

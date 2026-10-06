package hub

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetSpokeImageTagCache isolates each test from the package-level
// availability cache and restores whatever was there afterwards.
func resetSpokeImageTagCache(t *testing.T) {
	t.Helper()
	spokeImageTagAvailabilityMu.Lock()
	saved := spokeImageTagAvailabilityCache
	spokeImageTagAvailabilityCache = map[string]spokeImageTagAvailability{}
	spokeImageTagAvailabilityMu.Unlock()
	t.Cleanup(func() {
		spokeImageTagAvailabilityMu.Lock()
		spokeImageTagAvailabilityCache = saved
		spokeImageTagAvailabilityMu.Unlock()
	})
}

func cachedSpokeImageTagEntry(tag string) (spokeImageTagAvailability, bool) {
	spokeImageTagAvailabilityMu.Lock()
	defer spokeImageTagAvailabilityMu.Unlock()
	v, ok := spokeImageTagAvailabilityCache[tag]
	return v, ok
}

// fakeGHCR stands in for ghcr.io: tokenStatus/tokenBody drive the /token
// exchange, manifestStatus drives the HEAD on the manifest. It records the
// last manifest request so callers can assert on auth and path.
type fakeGHCR struct {
	t           *testing.T
	tokenStatus int
	tokenBody   string
	// manifestStatus is read by the handler goroutine and flipped by tests
	// between requests, so it is atomic to stay clean under -race.
	manifestStatus atomic.Int32

	tokenCalls    atomic.Int32
	manifestCalls atomic.Int32
	lastManifest  atomic.Pointer[http.Request]
}

func newFakeGHCR(t *testing.T, tokenStatus int, tokenBody string, manifestStatus int) *fakeGHCR {
	f := &fakeGHCR{t: t, tokenStatus: tokenStatus, tokenBody: tokenBody}
	f.manifestStatus.Store(int32(manifestStatus))
	return f
}

func (f *fakeGHCR) install(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			f.tokenCalls.Add(1)
			if scope := r.URL.Query().Get("scope"); scope != "repository:"+ghcrRepoSpoke+":pull" {
				f.t.Errorf("token scope = %q", scope)
			}
			w.WriteHeader(f.tokenStatus)
			_, _ = io.WriteString(w, f.tokenBody)
		case strings.Contains(r.URL.Path, "/manifests/"):
			f.manifestCalls.Add(1)
			req := r.Clone(r.Context())
			f.lastManifest.Store(req)
			w.WriteHeader(int(f.manifestStatus.Load()))
		default:
			f.t.Errorf("unexpected registry request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	savedBase := ghcrBase
	ghcrBase = srv.URL
	t.Cleanup(func() { ghcrBase = savedBase })
	return srv
}

func quietSlog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCachedSpokeImageTagExistsEmptyTag(t *testing.T) {
	resetSpokeImageTagCache(t)
	for _, tag := range []string{"", "   "} {
		if exists, verified := cachedSpokeImageTagExists(tag, nil); exists || verified {
			t.Fatalf("tag %q: exists=%v verified=%v, want false/false", tag, exists, verified)
		}
	}
	if _, ok := cachedSpokeImageTagEntry(""); ok {
		t.Fatal("empty tag must not be cached")
	}
}

func TestProbeSpokeImageTagFoundSendsBearerAndShortTag(t *testing.T) {
	resetSpokeImageTagCache(t)
	ghcr := newFakeGHCR(t, http.StatusOK, `{"token":"tok-123"}`, http.StatusOK)
	ghcr.install(t)

	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	exists, verified := cachedSpokeImageTagExists(fullSHA, quietSlog())
	if !exists || !verified {
		t.Fatalf("exists=%v verified=%v, want true/true", exists, verified)
	}
	req := ghcr.lastManifest.Load()
	if req == nil {
		t.Fatal("manifest was never requested")
	}
	if req.Method != http.MethodHead {
		t.Fatalf("manifest method = %s, want HEAD", req.Method)
	}
	wantPath := "/v2/" + ghcrRepoSpoke + "/manifests/" + shortSHA(fullSHA)
	if req.URL.Path != wantPath {
		t.Fatalf("manifest path = %q, want %q (tag must be shortened)", req.URL.Path, wantPath)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if accept := req.Header.Get("Accept"); !strings.Contains(accept, "application/vnd.oci.image.index.v1+json") {
		t.Fatalf("Accept = %q, want OCI index media type", accept)
	}

	entry, ok := cachedSpokeImageTagEntry(shortSHA(fullSHA))
	if !ok || !entry.exists {
		t.Fatalf("positive result must be cached under the short tag, got %+v ok=%v", entry, ok)
	}
}

func TestCachedSpokeImageTagExistsPositiveHitNeverReprobes(t *testing.T) {
	resetSpokeImageTagCache(t)
	ghcr := newFakeGHCR(t, http.StatusOK, `{"token":"t"}`, http.StatusOK)
	srv := ghcr.install(t)

	if exists, verified := cachedSpokeImageTagExists("abc1234", nil); !exists || !verified {
		t.Fatalf("first probe exists=%v verified=%v", exists, verified)
	}
	// Even a stale positive entry is authoritative: an image that exists never
	// disappears, so the registry must not be consulted again.
	spokeImageTagAvailabilityMu.Lock()
	spokeImageTagAvailabilityCache["abc1234"] = spokeImageTagAvailability{exists: true, at: time.Now().Add(-24 * time.Hour)}
	spokeImageTagAvailabilityMu.Unlock()
	srv.Close()

	if exists, verified := cachedSpokeImageTagExists("abc1234", nil); !exists || !verified {
		t.Fatalf("cached positive exists=%v verified=%v, want true/true", exists, verified)
	}
	if n := ghcr.tokenCalls.Load(); n != 1 {
		t.Fatalf("token requests = %d, want exactly 1 (second call must hit the cache)", n)
	}
}

func TestCachedSpokeImageTagExistsNegativeCachedUntilTTL(t *testing.T) {
	resetSpokeImageTagCache(t)
	ghcr := newFakeGHCR(t, http.StatusOK, `{"token":"t"}`, http.StatusNotFound)
	ghcr.install(t)

	if exists, verified := cachedSpokeImageTagExists("def5678", quietSlog()); exists || !verified {
		t.Fatalf("404 probe exists=%v verified=%v, want false/true", exists, verified)
	}
	entry, ok := cachedSpokeImageTagEntry("def5678")
	if !ok || entry.exists {
		t.Fatalf("negative result must be cached, got %+v ok=%v", entry, ok)
	}

	if exists, verified := cachedSpokeImageTagExists("def5678", quietSlog()); exists || !verified {
		t.Fatalf("within TTL exists=%v verified=%v, want false/true", exists, verified)
	}
	if n := ghcr.manifestCalls.Load(); n != 1 {
		t.Fatalf("manifest HEADs = %d, want 1 (negative entry inside TTL must be served from cache)", n)
	}

	// Age the entry past the negative TTL: the next lookup must re-probe, and
	// if the image has since been published the cache flips to positive.
	spokeImageTagAvailabilityMu.Lock()
	spokeImageTagAvailabilityCache["def5678"] = spokeImageTagAvailability{exists: false, at: time.Now().Add(-channelImageNegativeTTL - time.Second)}
	spokeImageTagAvailabilityMu.Unlock()
	ghcr.manifestStatus.Store(http.StatusOK)

	if exists, verified := cachedSpokeImageTagExists("def5678", quietSlog()); !exists || !verified {
		t.Fatalf("after TTL exists=%v verified=%v, want true/true", exists, verified)
	}
	if n := ghcr.manifestCalls.Load(); n != 2 {
		t.Fatalf("manifest HEADs = %d, want 2 (expired negative must re-probe)", n)
	}
	if entry, _ := cachedSpokeImageTagEntry("def5678"); !entry.exists {
		t.Fatalf("cache must flip to positive after re-probe, got %+v", entry)
	}
}

func TestProbeSpokeImageTagUnverifiedOutcomesAreNotCached(t *testing.T) {
	cases := []struct {
		name           string
		tokenStatus    int
		tokenBody      string
		manifestStatus int
	}{
		{"token rate-limited", http.StatusTooManyRequests, "", http.StatusOK},
		{"token non-OK", http.StatusInternalServerError, "", http.StatusOK},
		{"token undecodable", http.StatusOK, "not json", http.StatusOK},
		{"manifest rate-limited", http.StatusOK, `{"token":"t"}`, http.StatusTooManyRequests},
		{"manifest non-OK", http.StatusOK, `{"token":"t"}`, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetSpokeImageTagCache(t)
			ghcr := newFakeGHCR(t, tc.tokenStatus, tc.tokenBody, tc.manifestStatus)
			ghcr.install(t)
			// Both logger shapes: nil must not panic, non-nil must not fail.
			for _, logger := range []*slog.Logger{nil, quietSlog()} {
				exists, verified := cachedSpokeImageTagExists("fed9876", logger)
				if exists || verified {
					t.Fatalf("exists=%v verified=%v, want false/false", exists, verified)
				}
			}
			if _, ok := cachedSpokeImageTagEntry("fed9876"); ok {
				t.Fatal("an unverified probe must not populate the cache")
			}
			if n := ghcr.tokenCalls.Load(); n != 2 {
				t.Fatalf("token requests = %d, want 2 (nothing cached, so every call probes)", n)
			}
		})
	}
}

func TestProbeSpokeImageTagRegistryUnreachable(t *testing.T) {
	resetSpokeImageTagCache(t)
	ghcr := newFakeGHCR(t, http.StatusOK, `{"token":"t"}`, http.StatusOK)
	srv := ghcr.install(t)
	srv.Close() // connection refused on the token exchange

	for _, logger := range []*slog.Logger{nil, quietSlog()} {
		exists, verified := probeSpokeImageTag("abc1234", logger)
		if exists || verified {
			t.Fatalf("unreachable registry exists=%v verified=%v, want false/false", exists, verified)
		}
	}
	if _, ok := cachedSpokeImageTagEntry("abc1234"); ok {
		t.Fatal("a transport failure must not populate the cache")
	}
}

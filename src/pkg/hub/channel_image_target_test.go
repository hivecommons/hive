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

func resetSpokeImageTagCache(t *testing.T) {
	t.Helper()
	clear := func() {
		spokeImageTagAvailabilityMu.Lock()
		spokeImageTagAvailabilityCache = map[string]spokeImageTagAvailability{}
		spokeImageTagAvailabilityMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func useGhcrBase(t *testing.T, url string) {
	t.Helper()
	saved := ghcrBase
	ghcrBase = url
	t.Cleanup(func() { ghcrBase = saved })
}

func spokeImageCacheEntry(tag string) (spokeImageTagAvailability, bool) {
	spokeImageTagAvailabilityMu.Lock()
	defer spokeImageTagAvailabilityMu.Unlock()
	v, ok := spokeImageTagAvailabilityCache[tag]
	return v, ok
}

func setSpokeImageCacheEntry(tag string, v spokeImageTagAvailability) {
	spokeImageTagAvailabilityMu.Lock()
	spokeImageTagAvailabilityCache[tag] = v
	spokeImageTagAvailabilityMu.Unlock()
}

func TestCachedSpokeImageTagExistsBlankTag(t *testing.T) {
	resetSpokeImageTagCache(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	useGhcrBase(t, srv.URL)

	for _, tag := range []string{"", "   "} {
		exists, verified := cachedSpokeImageTagExists(tag, targetingLogger())
		if exists || verified {
			t.Errorf("tag %q: got (%v,%v), want (false,false)", tag, exists, verified)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("registry hit %d times, want 0", hits.Load())
	}
	if _, ok := spokeImageCacheEntry(""); ok {
		t.Error("blank tag must not be cached")
	}
}

func TestCachedSpokeImageTagExistsPublishedImage(t *testing.T) {
	resetSpokeImageTagCache(t)
	const fullSHA = "526ef71f0e1d2c3b4a5968526ef71f0e1d2c3b4a"
	short := shortSHA(fullSHA)
	var manifestHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if got := r.URL.Query().Get("scope"); got != "repository:"+ghcrRepoSpoke+":pull" {
				t.Errorf("token scope = %q", got)
			}
			_, _ = io.WriteString(w, `{"token":"exchanged"}`)
		case "/v2/" + ghcrRepoSpoke + "/manifests/" + short:
			manifestHits.Add(1)
			if r.Method != http.MethodHead {
				t.Errorf("method = %s, want HEAD", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer exchanged" {
				t.Errorf("Authorization = %q", got)
			}
			if got := r.Header.Get("Accept"); got == "" || !strings.Contains(got, "application/vnd.oci.image.index.v1+json") {
				t.Errorf("Accept = %q, want OCI index", got)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected registry request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	useGhcrBase(t, srv.URL)

	exists, verified := cachedSpokeImageTagExists(fullSHA, targetingLogger())
	if !exists || !verified {
		t.Fatalf("got (%v,%v), want (true,true)", exists, verified)
	}
	if v, ok := spokeImageCacheEntry(short); !ok || !v.exists {
		t.Errorf("cache entry for %q = %+v, ok=%v; want positive", short, v, ok)
	}

	exists, verified = cachedSpokeImageTagExists(short, targetingLogger())
	if !exists || !verified {
		t.Errorf("second call got (%v,%v), want (true,true)", exists, verified)
	}
	if manifestHits.Load() != 1 {
		t.Errorf("manifest probed %d times, want 1", manifestHits.Load())
	}
}

func TestCachedSpokeImageTagExistsPositiveCacheNeverReprobes(t *testing.T) {
	resetSpokeImageTagCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	useGhcrBase(t, srv.URL)

	setSpokeImageCacheEntry("abc1234", spokeImageTagAvailability{exists: true, at: time.Now().Add(-24 * time.Hour)})
	exists, verified := cachedSpokeImageTagExists("abc1234", targetingLogger())
	if !exists || !verified {
		t.Errorf("got (%v,%v), want (true,true) from cache", exists, verified)
	}
}

func TestCachedSpokeImageTagExistsNegativeCacheAndTTL(t *testing.T) {
	resetSpokeImageTagCache(t)
	var published atomic.Bool
	var manifestHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		manifestHits.Add(1)
		if published.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	useGhcrBase(t, srv.URL)

	const tag = "def5678"
	exists, verified := cachedSpokeImageTagExists(tag, targetingLogger())
	if exists || !verified {
		t.Fatalf("got (%v,%v), want (false,true)", exists, verified)
	}
	if v, ok := spokeImageCacheEntry(tag); !ok || v.exists {
		t.Fatalf("expected negative cache entry, got %+v ok=%v", v, ok)
	}

	published.Store(true)
	exists, verified = cachedSpokeImageTagExists(tag, targetingLogger())
	if exists || !verified {
		t.Errorf("inside TTL got (%v,%v), want cached (false,true)", exists, verified)
	}
	if manifestHits.Load() != 1 {
		t.Errorf("manifest probed %d times inside TTL, want 1", manifestHits.Load())
	}

	setSpokeImageCacheEntry(tag, spokeImageTagAvailability{exists: false, at: time.Now().Add(-channelImageNegativeTTL - time.Second)})
	exists, verified = cachedSpokeImageTagExists(tag, targetingLogger())
	if !exists || !verified {
		t.Errorf("after TTL got (%v,%v), want (true,true)", exists, verified)
	}
	if manifestHits.Load() != 2 {
		t.Errorf("manifest probed %d times, want 2", manifestHits.Load())
	}
	if v, _ := spokeImageCacheEntry(tag); !v.exists {
		t.Error("cache entry should have flipped positive")
	}
}

func TestCachedSpokeImageTagExistsUnverifiedNotCached(t *testing.T) {
	cases := []struct {
		name  string
		token func(w http.ResponseWriter)
		head  int
	}{
		{"token 429", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }, 0},
		{"token 5xx", func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }, 0},
		{"token non-JSON", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "not json") }, 0},
		{"manifest 429", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"token":"t"}`) }, http.StatusTooManyRequests},
		{"manifest 5xx", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"token":"t"}`) }, http.StatusInternalServerError},
	}
	loggers := map[string]*slog.Logger{"nil logger": nil, "logger": targetingLogger()}
	for _, tc := range cases {
		for lname, logger := range loggers {
			t.Run(tc.name+"/"+lname, func(t *testing.T) {
				resetSpokeImageTagCache(t)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/token" {
						tc.token(w)
						return
					}
					w.WriteHeader(tc.head)
				}))
				t.Cleanup(srv.Close)
				useGhcrBase(t, srv.URL)

				exists, verified := cachedSpokeImageTagExists("aaa1111", logger)
				if exists || verified {
					t.Errorf("got (%v,%v), want (false,false)", exists, verified)
				}
				if _, ok := spokeImageCacheEntry("aaa1111"); ok {
					t.Error("unverified result must not be cached")
				}
			})
		}
	}
}

func TestCachedSpokeImageTagExistsRegistryUnreachable(t *testing.T) {
	for lname, logger := range map[string]*slog.Logger{"nil logger": nil, "logger": targetingLogger()} {
		t.Run(lname, func(t *testing.T) {
			resetSpokeImageTagCache(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			srv.Close()
			useGhcrBase(t, srv.URL)

			exists, verified := cachedSpokeImageTagExists("bbb2222", logger)
			if exists || verified {
				t.Errorf("got (%v,%v), want (false,false)", exists, verified)
			}
			if _, ok := spokeImageCacheEntry("bbb2222"); ok {
				t.Error("unreachable registry must not be cached")
			}
		})
	}
}

func TestProbeSpokeImageTagManifestTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = io.WriteString(w, `{"token":"t"}`)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack unsupported")
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	useGhcrBase(t, srv.URL)

	for _, logger := range []*slog.Logger{nil, targetingLogger()} {
		exists, verified := probeSpokeImageTag("ccc3333", logger)
		if exists || verified {
			t.Errorf("got (%v,%v), want (false,false)", exists, verified)
		}
	}
}

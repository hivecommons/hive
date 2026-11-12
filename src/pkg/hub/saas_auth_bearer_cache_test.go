package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func withBearerProbeServer(t *testing.T, handler http.HandlerFunc) *int32 {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		handler(w, r)
	}))
	oldURL := defaultGHUserURL
	defaultGHUserURL = srv.URL + "/user"
	t.Cleanup(func() {
		defaultGHUserURL = oldURL
		srv.Close()
	})
	return &hits
}

func clearGHTokenCache(t *testing.T, tokens ...string) {
	t.Helper()
	clear := func() {
		ghTokenCacheMu.Lock()
		for _, tok := range tokens {
			delete(ghTokenCache, tok)
		}
		ghTokenCacheMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// A rejected bearer token must not re-probe GET /user on every request while
// the negative entry is fresh: that re-probe charged the token owner's own
// core quota while it was already exhausted (the 401 storm in the PAT outage).
func TestValidateGitHubToken_RejectedTokenIsNegativelyCached(t *testing.T) {
	hits := withBearerProbeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	const token = "dead-token-negative-cache"
	clearGHTokenCache(t, token)

	srv := newHubServerForTest(t)
	for i := 0; i < 3; i++ {
		if got := srv.validateGitHubToken(token); got != "" {
			t.Fatalf("call %d: rejected token resolved to %q", i, got)
		}
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("GET /user probed %d times for a rejected token; want 1", n)
	}
	ghTokenCacheMu.RLock()
	entry, ok := ghTokenCache[token]
	ghTokenCacheMu.RUnlock()
	if !ok || entry.username != "" || !entry.expiresAt.After(time.Now()) {
		t.Fatalf("negative entry = %+v, ok=%v; want an unexpired entry with empty username", entry, ok)
	}
}

func TestValidateGitHubToken_CacheCapacityAndExpiry(t *testing.T) {
	ghTokenCacheMu.Lock()
	original := ghTokenCache
	ghTokenCache = make(map[string]ghTokenCacheEntry)
	ghTokenCacheMu.Unlock()
	t.Cleanup(func() {
		ghTokenCacheMu.Lock()
		ghTokenCache = original
		ghTokenCacheMu.Unlock()
	})

	hits := withBearerProbeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer valid-overflow" {
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "valid-user"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := newHubServerForTest(t)
	for i := range ghTokenCacheMaxEntries + 1 {
		token := "rejected-capacity-" + strconv.Itoa(i)
		if got := srv.validateGitHubToken(token); got != "" {
			t.Fatalf("rejected token resolved to %q", got)
		}
	}
	ghTokenCacheMu.RLock()
	size := len(ghTokenCache)
	ghTokenCacheMu.RUnlock()
	if size != ghTokenCacheMaxEntries {
		t.Fatalf("cache has %d entries, want %d", size, ghTokenCacheMaxEntries)
	}

	// Both rejected and accepted tokens still probe when the cache is full.
	for range 2 {
		if got := srv.validateGitHubToken("rejected-capacity-" + strconv.Itoa(ghTokenCacheMaxEntries)); got != "" {
			t.Fatalf("overflow rejected token resolved to %q", got)
		}
		if got := srv.validateGitHubToken("valid-overflow"); got != "valid-user" {
			t.Fatalf("overflow accepted token resolved to %q", got)
		}
	}
	if got, want := atomic.LoadInt32(hits), int32(ghTokenCacheMaxEntries+5); got != want {
		t.Fatalf("GET /user probed %d times, want %d", got, want)
	}
	ghTokenCacheMu.Lock()
	entry := ghTokenCache["rejected-capacity-0"]
	entry.expiresAt = time.Now().Add(-time.Second)
	ghTokenCache["rejected-capacity-0"] = entry
	ghTokenCacheMu.Unlock()

	// A full cache sweeps expired entries and makes room for a new positive entry.
	for range 2 {
		if got := srv.validateGitHubToken("valid-overflow"); got != "valid-user" {
			t.Fatalf("accepted token after expiry resolved to %q", got)
		}
	}
	if got, want := atomic.LoadInt32(hits), int32(ghTokenCacheMaxEntries+6); got != want {
		t.Fatalf("GET /user probed %d times after expiry, want %d", got, want)
	}
	ghTokenCacheMu.RLock()
	_, expiredPresent := ghTokenCache["rejected-capacity-0"]
	size = len(ghTokenCache)
	ghTokenCacheMu.RUnlock()
	if expiredPresent || size != ghTokenCacheMaxEntries {
		t.Fatalf("after sweep: expired present = %t, cache size = %d", expiredPresent, size)
	}
}

// An expired negative entry probes again, and a token that has since become
// valid resolves — the negative cache is a short hold, not a ban.
func TestValidateGitHubToken_ExpiredNegativeEntryReprobes(t *testing.T) {
	hits := withBearerProbeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "revived"})
	})
	const token = "revived-token"
	clearGHTokenCache(t, token)
	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{expiresAt: time.Now().Add(-time.Second)}
	ghTokenCacheMu.Unlock()

	srv := newHubServerForTest(t)
	if got := srv.validateGitHubToken(token); got != "revived" {
		t.Fatalf("validateGitHubToken = %q, want revived", got)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("GET /user probed %d times; want 1", n)
	}
}

func TestGhTokenNegativeExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	hdr := func(remaining string, reset time.Time) http.Header {
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", remaining)
		h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		return h
	}
	cases := []struct {
		name   string
		status int
		hdr    http.Header
		want   time.Time
	}{
		{"401 uses the short hold", http.StatusUnauthorized, http.Header{}, now.Add(ghTokenNegativeTTL)},
		{"403 with quota left is not a rate limit", http.StatusForbidden, hdr("12", now.Add(time.Hour)), now.Add(ghTokenNegativeTTL)},
		{"403 exhausted holds until reset", http.StatusForbidden, hdr("0", now.Add(5*time.Minute)), now.Add(5 * time.Minute)},
		{"429 exhausted holds until reset", http.StatusTooManyRequests, hdr("0", now.Add(3*time.Minute)), now.Add(3 * time.Minute)},
		{"reset far away is capped", http.StatusForbidden, hdr("0", now.Add(2*time.Hour)), now.Add(ghTokenNegativeMaxTTL)},
		{"reset already passed keeps the short hold", http.StatusForbidden, hdr("0", now.Add(-time.Minute)), now.Add(ghTokenNegativeTTL)},
		{"garbage reset keeps the short hold", http.StatusForbidden, func() http.Header {
			h := http.Header{}
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset", "soon")
			return h
		}(), now.Add(ghTokenNegativeTTL)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghTokenNegativeExpiry(tc.status, tc.hdr, now); !got.Equal(tc.want) {
				t.Fatalf("ghTokenNegativeExpiry = %v, want %v", got, tc.want)
			}
		})
	}
}

// The probe is attributed in /api/gh-rate-limits under the pass-through
// ("agent:") caller so an exhausted user token is visible on the hub admin
// page without being mistaken for the hub's own App credential.
func TestValidateGitHubToken_ProbeIsAccountedAsPassThroughCaller(t *testing.T) {
	withBearerProbeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	})
	const token = "accounted-probe-token"
	clearGHTokenCache(t, token)

	srv := newHubServerForTest(t)
	if got := srv.validateGitHubToken(token); got != "" {
		t.Fatalf("validateGitHubToken = %q, want empty", got)
	}
	var found *hgithub.RESTConsumer
	for _, c := range hgithub.RESTTopConsumers(100) {
		if c.Caller == ghTokenValidateCaller && c.Endpoint == "/user" {
			c := c
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatalf("caller %q not found in REST accounting: %+v", ghTokenValidateCaller, hgithub.RESTTopConsumers(100))
	}
	if found.RateLimited == 0 {
		t.Fatalf("probe not recorded as rate-limited: %+v", *found)
	}
}

package github

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// These tests lock in the fix for the dashboard's github_auth health check
// flapping red↔green on a hive whose GitHub App is genuinely working.
//
// Root cause: Token() serves the cached installation token only while
// now < tokenExpiry-tokenRefreshBuffer. Once inside the final buffer window it
// makes a live CreateInstallationToken network call on EVERY call. On
// GHE-adjacent / firewalled clusters that call intermittently blips (transient
// 5xx / TLS / network). The old code returned that error even though the
// cached token was still valid, so the once-per-heartbeat health check flipped
// github_auth to fail and back — Degraded rabbiting on a working hive.
//
// The fix: a proactive-refresh blip falls back to the still-valid cached token.
// A genuine failure (no cached token, or the cached token has truly expired)
// must still error so a real outage still shows Degraded.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestToken_TransientMintErrorServesValidCache is the core false-flap case:
// we are inside the refresh buffer (so Token() attempts a re-mint), the mint
// blips (500), but the cached token is still valid — Token() must return the
// cached token, NOT an error.
func TestToken_TransientMintErrorServesValidCache(t *testing.T) {
	key, _ := generateTestKey(t)

	// GitHub mint endpoint always fails (simulates a transient blip).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream hiccup", http.StatusInternalServerError)
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		cachedToken:    "still-valid-cached-token",
		// Inside the refresh buffer window (< tokenRefreshBuffer to expiry) but
		// NOT yet expired: the token is still perfectly usable.
		tokenExpiry: time.Now().Add(tokenRefreshBuffer / 2),
	}

	token, err := auth.Token(context.Background())
	if err != nil {
		t.Fatalf("Token returned error on a transient mint blip while cache is still valid: %v", err)
	}
	if token != "still-valid-cached-token" {
		t.Errorf("token = %q, want still-valid-cached-token (should serve cache through the blip)", token)
	}
}

// TestToken_SustainedFailureNoCacheStillErrors proves the fix does not mask a
// real failure: with no cached token, a failing mint must error.
func TestToken_SustainedFailureNoCacheStillErrors(t *testing.T) {
	key, _ := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "app not installed", http.StatusUnauthorized)
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		// No cached token at all.
	}

	if _, err := auth.Token(context.Background()); err == nil {
		t.Fatal("expected an error when the App is genuinely broken and there is no cached token")
	}
}

// TestToken_ExpiredCacheFailingMintStillErrors proves that once the cached
// token has TRULY expired, a failing mint still errors — an expired token is
// not a usable credential, so falling back to it would hide a real outage.
func TestToken_ExpiredCacheFailingMintStillErrors(t *testing.T) {
	key, _ := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream hiccup", http.StatusInternalServerError)
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		cachedToken:    "long-expired-token",
		tokenExpiry:    time.Now().Add(-time.Minute), // already expired
	}

	if _, err := auth.Token(context.Background()); err == nil {
		t.Fatal("expected an error when the cached token has expired and the mint keeps failing")
	}
}

// TestToken_TransientThenRecoveryDoesNotFlap simulates the live sequence: a
// heartbeat mints, the next hits a blip (must not error), and the following one
// recovers with a fresh token. The health check would see pass→pass→pass, never
// flipping to fail — i.e. no rabbiting on a working hive.
func TestToken_TransientThenRecoveryDoesNotFlap(t *testing.T) {
	key, _ := generateTestKey(t)

	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "blip", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"fresh-token","expires_at":"` +
			time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		cachedToken:    "boot-token",
		// Inside the buffer window so every call attempts a re-mint.
		tokenExpiry: time.Now().Add(tokenRefreshBuffer / 2),
	}

	// 1) Blip: must serve the still-valid cached token, no error.
	fail.Store(true)
	tok, err := auth.Token(context.Background())
	if err != nil {
		t.Fatalf("blip poll errored instead of serving cache: %v", err)
	}
	if tok != "boot-token" {
		t.Errorf("blip poll token = %q, want boot-token", tok)
	}

	// 2) Recovery: once the retry interval has elapsed the mint succeeds and
	// the cache advances to the fresh token.
	fail.Store(false)
	auth.refreshRetryAt = time.Now().Add(-time.Second)
	tok, err = auth.Token(context.Background())
	if err != nil {
		t.Fatalf("recovery poll errored: %v", err)
	}
	if tok != "fresh-token" {
		t.Errorf("recovery poll token = %q, want fresh-token", tok)
	}
}

// TestToken_FailedRefreshBacksOff locks in hivecommons/hive#19706: after a
// proactive refresh fails, Token() must serve the still-valid cached token
// WITHOUT re-minting until tokenRefreshRetryInterval passes. Re-minting on
// every call held a.mu for up to tokenMintTimeout per caller while GitHub was
// unreachable and logged one WARN per call. The backoff must never extend an
// expired token's life.
func TestToken_FailedRefreshBacksOff(t *testing.T) {
	key, _ := generateTestKey(t)

	var mints atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		http.Error(w, "upstream hiccup", http.StatusInternalServerError)
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		cachedToken:    "still-valid-cached-token",
		tokenExpiry:    time.Now().Add(tokenRefreshBuffer / 2),
	}

	for i := range 5 {
		tok, err := auth.Token(context.Background())
		if err != nil || tok != "still-valid-cached-token" {
			t.Fatalf("call %d: Token() = %q, %v; want the cached token", i, tok, err)
		}
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("mint attempts during backoff = %d, want 1", got)
	}

	// Retry interval elapsed: the next call tries the mint again.
	auth.refreshRetryAt = time.Now().Add(-time.Second)
	if _, err := auth.Token(context.Background()); err != nil {
		t.Fatalf("Token() after backoff: %v", err)
	}
	if got := mints.Load(); got != 2 {
		t.Fatalf("mint attempts after backoff = %d, want 2", got)
	}

	// A pending backoff must not serve a token that has truly expired.
	auth.tokenExpiry = time.Now().Add(-time.Second)
	auth.refreshRetryAt = time.Now().Add(time.Minute)
	if _, err := auth.Token(context.Background()); err == nil {
		t.Fatal("Token() served an expired cached token during backoff")
	}
	if got := mints.Load(); got != 3 {
		t.Fatalf("mint attempts with expired token = %d, want 3", got)
	}
}

// TestToken_SharedCacheIsOwnerOnly locks in audit H3 (CWE-522/732): the SHARED,
// full-privilege installation-token cache (TokenCachePath / DocsTokenCachePath,
// written by Token() with tokenCachePerms) MUST be owner-only 0600. The hive
// process runs as "dev" and every agent UID is in the shared "node" group, so
// any group/other read bit here would leak the full installation token to every
// agent and defeat per-agent tier scoping. The per-agent scoped caches are a
// SEPARATE path (WriteAgentToken / agentTokenCachePerms) and are intentionally
// 0640 dev:hive-<agent>; this test guards only the shared cache.
func TestToken_SharedCacheIsOwnerOnly(t *testing.T) {
	key, _ := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"fresh-token","expires_at":"` +
			time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()

	cachePath := t.TempDir() + "/gh-app-token.cache"
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      cachePath,
		// No cached token → the first call mints and writes the cache.
	}

	if _, err := auth.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}

	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat shared cache: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("shared full-token cache mode = %o, want 0600 (audit H3: must NOT be readable by the shared 'node' group)", got)
	}
	// Belt-and-suspenders: assert no group/other bits regardless of the exact mode.
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("shared full-token cache mode %o leaks read/write beyond the owner", info.Mode().Perm())
	}
}

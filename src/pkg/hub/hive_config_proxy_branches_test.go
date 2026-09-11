package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests cover the handleProxyHiveConfig branches that run AFTER the
// registry lookup and the F9 ownership gate: the SSRF guard refusal, the
// authorized proxy fetch (success and upstream-status passthrough), the
// unreachable-upstream 502, and the no-redirect-follow guarantee. Existing
// tests (hub_saas_handlers_test.go, hub_coverage_extra_test.go) stop at the
// 404 and ownerless-403 gates, so everything past line ~7308 of saas.go was
// unexercised.

// proxyConfigServer builds a hub server with one owned registry hive whose
// DashboardURL points at upstream, plus a mux that binds the {hiveID} path
// value the way production routing does.
func proxyConfigServer(t *testing.T, owner, upstreamURL string) *http.ServeMux {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	srv := newHubServerForTest(t)
	srv.setHubSecret(testHubSecret)
	mkUser(t, owner)

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "cfg-hive", Owner: owner, DashboardURL: upstreamURL},
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", srv.handleProxyHiveConfig)
	return mux
}

// TestHandleProxyHiveConfigSSRFGuardRefuses pins that a DashboardURL the SSRF
// guard rejects is refused with 403 BEFORE any fetch — even for the hive's
// owner. The guard is the only thing standing between a self-reported spoke
// URL and a server-side fetch of e.g. 169.254.169.254.
func TestHandleProxyHiveConfigSSRFGuardRefuses(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	// Production guard behavior: loopback (the httptest server) is private,
	// so the real isPrivateURL default refuses it. Assert with the guard in
	// its production configuration — no override.
	mux := proxyConfigServer(t, "alice", upstream.URL)

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("SSRF-guarded URL: expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
	if reached {
		t.Error("upstream was fetched despite the SSRF guard — the guard is not in front of the fetch")
	}
}

// TestHandleProxyHiveConfigOwnerSuccess covers the authorized happy path: the
// hive's owner pulls the config, the hub proxies the upstream body and
// content type back verbatim.
func TestHandleProxyHiveConfigOwnerSuccess(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	const yaml = "agents:\n  scanner:\n    model: claude-sonnet\n"
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(yaml))
	}))
	defer upstream.Close()

	mux := proxyConfigServer(t, "alice", upstream.URL+"/") // trailing slash: TrimRight must not double the path separator

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("owner fetch: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if gotPath != "/api/config/download" {
		t.Errorf("upstream path = %q, want /api/config/download", gotPath)
	}
	if got := w.Header().Get("Content-Type"); got != "application/x-yaml" {
		t.Errorf("Content-Type = %q, want application/x-yaml", got)
	}
	if w.Body.String() != yaml {
		t.Errorf("body = %q, want the upstream yaml verbatim", w.Body.String())
	}
}

// TestHandleProxyHiveConfigUpstreamStatusPassthrough pins that a non-200
// upstream status is passed through unchanged rather than being remapped —
// the spoke's own 500 must stay distinguishable from the hub's 502.
func TestHandleProxyHiveConfigUpstreamStatusPassthrough(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("spoke exploded"))
	}))
	defer upstream.Close()

	mux := proxyConfigServer(t, "alice", upstream.URL)

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("upstream 500: expected passthrough 500, got %d", w.Code)
	}
	if w.Body.String() != "spoke exploded" {
		t.Errorf("body = %q, want upstream body verbatim", w.Body.String())
	}
}

// TestHandleProxyHiveConfigUpstreamUnreachable covers the fetch-error branch:
// a DashboardURL nothing listens on yields 502, not a hang or a panic.
func TestHandleProxyHiveConfigUpstreamUnreachable(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	// Reserve an address, then close it so the dial reliably fails fast.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	mux := proxyConfigServer(t, "alice", deadURL)

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream: expected 502, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestHandleProxyHiveConfigDoesNotFollowRedirects pins the CheckRedirect
// guard: a 30x from the spoke is returned to the caller as-is, never
// followed — following it could re-open the SSRF the guard closed by
// bouncing the hub to an internal host.
func TestHandleProxyHiveConfigDoesNotFollowRedirects(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	redirectFollowed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal-secret" {
			redirectFollowed = true
			_, _ = w.Write([]byte("internal"))
			return
		}
		http.Redirect(w, r, "/internal-secret", http.StatusFound)
	}))
	defer upstream.Close()

	mux := proxyConfigServer(t, "alice", upstream.URL)

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if redirectFollowed {
		t.Error("hub followed a spoke redirect — SSRF re-opened past the guard")
	}
	if w.Code != http.StatusFound {
		t.Errorf("redirect response: expected passthrough 302, got %d", w.Code)
	}
}

// TestHandleProxyHiveConfigNonOwnerForbidden pins that an authenticated
// NON-owner (not admin) is refused even when the hive HAS an owner — the
// complement of the existing ownerless-entry F9 test.
func TestHandleProxyHiveConfigNonOwnerForbidden(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	mux := proxyConfigServer(t, "alice", upstream.URL)
	mkUser(t, "mallory")

	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("mallory"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-owner: expected 403, got %d", w.Code)
	}
	if reached {
		t.Error("upstream config was fetched for a non-owner — CWE-862 regression")
	}
}

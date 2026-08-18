package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsSameOriginAsHub is the request-authoring trust boundary (audit F4).
// Sibling tenants must NOT be trusted here — that was the vulnerability.
func TestIsSameOriginAsHub(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		// Positive controls: the legitimate hub origin and local dev must stay
		// trusted, so a handler that rejects everything cannot pass this test.
		{"https://hive.kubestellar.io", true},
		{"https://hive.kubestellar.io/dashboard", true},
		{"http://localhost", true},
		{"http://localhost:3001", true},
		{"http://127.0.0.1", true},
		// F4: sibling tenants are no longer request-authoring origins.
		{"https://hosted-test.hive.kubestellar.io", false},
		{"https://my.hosted.hive.kubestellar.io", false},
		{"https://attacker-hive.hive.kubestellar.io", false},
		// Unrelated / lookalike hosts stay rejected.
		{"https://evil.com", false},
		{"https://hive.kubestellar.io.evil.com", false},
		{"https://evil-hive.kubestellar.io", false},
		{"", false},
		{"not-a-url", false},
		{"https://localhost.evil.com", false},
	}
	for _, tt := range tests {
		got := isSameOriginAsHub(tt.origin)
		if got != tt.want {
			t.Errorf("isSameOriginAsHub(%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}

// TestIsTrustedRedirectTarget pins the OTHER half of the F4 split: post-login
// redirects must still reach hosted tenants, because every hive's ingress
// auth-signin bounces through the hub with redirect=https://<id>.hive...
// Narrowing this would break hosted sign-in.
func TestIsTrustedRedirectTarget(t *testing.T) {
	tests := []struct {
		target string
		want   bool
	}{
		{"https://hive.kubestellar.io", true},
		{"https://hosted-test.hive.kubestellar.io/dashboard", true},
		{"https://my.hosted.hive.kubestellar.io", true},
		{"http://localhost:3001", true},
		{"http://127.0.0.1", true},
		{"https://evil.com", false},
		{"https://hive.kubestellar.io.evil.com", false},
		{"https://evil-hive.kubestellar.io", false},
		{"", false},
		{"https://localhost.evil.com", false},
	}
	for _, tt := range tests {
		got := isTrustedRedirectTarget(tt.target)
		if got != tt.want {
			t.Errorf("isTrustedRedirectTarget(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
}

func TestIsCSRFSafe(t *testing.T) {
	tests := []struct {
		name   string
		method string
		origin string
		ct     string
		want   bool
	}{
		{"GET is safe", "GET", "", "", true},
		{"HEAD is safe", "HEAD", "", "", true},
		{"OPTIONS is safe", "OPTIONS", "", "", true},
		{"POST with trusted origin", "POST", "https://hive.kubestellar.io", "", true},
		{"POST with evil origin", "POST", "https://evil.com", "", false},
		// F4: a sibling tenant is a hostile origin for CSRF purposes even
		// though the browser hands it the parent-domain session cookie.
		{"POST from sibling tenant origin", "POST", "https://attacker-hive.hive.kubestellar.io", "", false},
		{"POST from sibling tenant origin with JSON ct", "POST", "https://attacker-hive.hive.kubestellar.io", "application/json", false},
		// AUDIT F4 — THIS CASE IS INVERTED, NOT RELAXED. It previously asserted
		// `true`: a POST with no Origin, no Referer and Content-Type
		// application/json was treated as CSRF-safe. That WAS the vulnerability,
		// written down as an expectation. Content-Type is attacker-controlled and
		// proves nothing, and "no Origin/Referer" is not evidence of a
		// non-browser caller — Referrer-Policy: no-referrer, privacy extensions
		// and corporate proxies strip these routinely. Failing closed is the
		// whole fix, so the expectation flips to `false`.
		{"POST with JSON content-type and no origin is NOT safe", "POST", "", "application/json", false},
		{"POST with no origin or ct", "POST", "", "", false},
		{"POST with evil origin suffix", "POST", "https://hive.kubestellar.io.evil.com", "", false},
		{"DELETE with trusted referer", "DELETE", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/test", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.ct != "" {
				req.Header.Set("Content-Type", tt.ct)
			}
			got := isCSRFSafe(req)
			if got != tt.want {
				t.Errorf("isCSRFSafe() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCSRFSafeReferer(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/test", nil)
	req.Header.Set("Referer", "https://hive.kubestellar.io/dashboard")
	if !isCSRFSafe(req) {
		t.Error("POST with trusted Referer should be safe")
	}

	req2 := httptest.NewRequest("POST", "/api/test", nil)
	req2.Header.Set("Referer", "https://evil.com/page")
	if isCSRFSafe(req2) {
		t.Error("POST with evil Referer should NOT be safe")
	}
}

func TestSecureCompareHub(t *testing.T) {
	if !secureCompareHub("abc123", "abc123") {
		t.Error("equal strings should match")
	}
	if secureCompareHub("abc123", "abc124") {
		t.Error("different strings should not match")
	}
	if secureCompareHub("abc", "abcd") {
		t.Error("different length strings should not match")
	}
	if !secureCompareHub("", "") {
		t.Error("empty strings should match (both empty)")
	}
}

func TestIsPrivateURL(t *testing.T) {
	// L3: the public-hostname rows below used to perform a REAL DNS lookup, so
	// this test failed closed in a network-isolated sandbox. Resolution is now
	// stubbed for exactly these three hosts; every private row is still decided
	// by the literal-prefix checks that run before resolution, and an
	// unregistered host still fails closed (see
	// TestStubPrivateURLResolverIsFailClosed). The guard itself is unchanged.
	stubPrivateURLResolver(t, "github.com", "api.github.com", "hive.kubestellar.io")

	tests := []struct {
		url  string
		want bool
	}{
		{"https://localhost/api", true},
		{"http://127.0.0.1:8080", true},
		{"http://10.0.0.1/path", true},
		{"http://192.168.1.1", true},
		{"http://172.16.0.1", true},
		{"http://169.254.1.1", true},
		{"http://[::1]:8080", true},         // IPv6 loopback — private
		{"http://[::ffff:127.0.0.1]", true}, // IPv4-mapped loopback — private
		{"http://0.0.0.0", true},
		{"https://github.com", false},
		{"https://api.github.com/repos", false},
		{"https://hive.kubestellar.io", false},
	}
	for _, tt := range tests {
		got := isPrivateURL(context.Background(), tt.url)
		if got != tt.want {
			t.Errorf("isPrivateURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestIsUnfurlBot(t *testing.T) {
	tests := []struct {
		ua   string
		want bool
	}{
		{"Slackbot-LinkExpanding 1.0", true},
		{"Twitterbot/1.0", true},
		{"facebookexternalhit/1.1", true},
		{"Mozilla/5.0 (compatible; Discordbot/2.0)", true},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", false},
		{"curl/7.68.0", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isUnfurlBot(tt.ua)
		if got != tt.want {
			t.Errorf("isUnfurlBot(%q) = %v, want %v", tt.ua, got, tt.want)
		}
	}
}

func TestNewHubServer(t *testing.T) {
	logger := slog.Default()
	srv := NewHubServer(8080, logger, "abc123", "v2")
	if srv == nil {
		t.Fatal("NewHubServer returned nil")
	}
	if srv.hubGitHash != "abc123" {
		t.Errorf("hubGitHash = %q, want %q", srv.hubGitHash, "abc123")
	}
}

func TestHandleRegistryEmpty(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/registry", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandleHealthCheck(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// The hub doesn't have a /api/health endpoint registered in the mux,
	// but the registry endpoint should work
	req := httptest.NewRequest("GET", "/api/registry", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestHandleHeartbeatNoAuth(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("secret123")
	req := httptest.NewRequest("POST", "/api/heartbeat", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestDecryptTokenInvalid(t *testing.T) {
	_, err := decryptToken("not-valid-base64-encrypted")
	if err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestHandleRegistryJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/registry", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "hives") {
		t.Error("response should contain hives key")
	}
}

func TestHandleHeartbeatBadJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 400 or 401", w.Code)
	}
}

func TestHandleHubVersion(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "abc123", "v2")
	req := httptest.NewRequest("GET", "/api/hub/version", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "abc123") {
		t.Errorf("version should contain git hash")
	}
}

func TestHandleLeaderboard(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/hub/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
}

func TestHandleStats(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/hub/stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
}

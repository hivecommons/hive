package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthCheck_SSOHandoffBypassesGate is the hub half of the redirect-loop fix.
//
// The spoke ingress puts every path behind nginx auth_request ->
// /api/saas/auth-check. That check authorizes against the hub's per-user grant
// list for the hive. The SSO handoff exists precisely to admit a hub-
// authenticated user who has no such grant row, so gating /sso on it 401s the
// requests the handoff exists to serve — and nginx converts that 401 into an
// auth-signin redirect to the hub login, which (with a valid hub cookie
// already present) immediately redirects back to /sso. Infinite bounce.
//
// /sso must therefore reach the spoke unauthenticated, exactly like the other
// public paths. This does NOT weaken auth: the signed, hive-scoped,
// short-lived HMAC token in the query is the credential, and the spoke
// verifies it plus its own allowlist before minting any session.
func TestAuthCheck_SSOHandoffBypassesGate(t *testing.T) {
	tests := []struct {
		name        string
		originalURI string
		wantStatus  int
	}{
		{
			name:        "bare sso path",
			originalURI: ssoHandoffPath,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "sso path carrying the handoff token",
			originalURI: ssoHandoffPath + "?token=eyJ2IjoiaGl2ZS1zc28tdjEifQ.sig",
			wantStatus:  http.StatusOK,
		},
		{
			// Everything else stays gated — the bypass is scoped to the handoff.
			name:        "dashboard root is still gated",
			originalURI: "/",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "api is still gated",
			originalURI: "/api/status",
			wantStatus:  http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewHubServer(0, slog.Default(), "test", "v2")
			req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test-hive", nil)
			req.Header.Set("X-Original-URI", tc.originalURI)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("auth-check for %q = %d, want %d", tc.originalURI, w.Code, tc.wantStatus)
			}
		})
	}
}

// TestAuthCheck_SSOBypassSetsNoProxyProof guards the F2 contract: the
// proof-of-proxy header is set ONLY on the authenticated success path, never on
// a public-path bypass. If the /sso bypass leaked the hive's dashboard token,
// anyone could fetch it by hitting the auth-check with X-Original-URI=/sso.
func TestAuthCheck_SSOBypassSetsNoProxyProof(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test-hive", nil)
	req.Header.Set("X-Original-URI", ssoHandoffPath)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if got := w.Header().Get(proxyAuthHeader); got != "" {
		t.Errorf("public-path bypass leaked %s = %q; it must only be set on the authenticated success path", proxyAuthHeader, got)
	}
	if got := w.Header().Get("X-Hive-User"); got != "" {
		t.Errorf("public-path bypass set X-Hive-User = %q; it must assert no identity", got)
	}
}

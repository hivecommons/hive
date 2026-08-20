package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOwnerOnlyAgentControlsRejectNonOwners(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"pause", "/api/pause/scanner", (&Server{}).handlePause},
		{"resume", "/api/resume/scanner", (&Server{}).handleResume},
		{"breaker engage", "/api/breaker/engage", (&Server{}).handleBreakerEngage},
		{"breaker release", "/api/breaker/release", (&Server{}).handleBreakerRelease},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Hive-Role", "read-write")
			req.SetPathValue("agent", "scanner")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("non-owner role status = %d, want 403", w.Code)
			}
		})
	}
}

// TestOwnerOnlyMutationsAllowSharedBearerToken pins the #4134 fix: the shared
// dashboard token IS the operator credential on token-secured spokes (the
// dashboard UI itself sends it as Authorization: Bearer from localStorage), so
// presenting it must grant the server-set owner role. Before the fix every
// owner-gated save (budget, pause, breaker...) failed with "owner access
// required" for the legitimate operator.
func TestOwnerOnlyMutationsAllowSharedBearerToken(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"pause", "/api/pause/scanner"},
		{"resume", "/api/resume/scanner"},
		{"breaker engage", "/api/breaker/engage"},
		{"breaker release", "/api/breaker/release"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFullServer(t)
			s.authToken = "shared-secret-token"
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+s.authToken)
			w := httptest.NewRecorder()

			s.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("shared-token operator request status = %d, want 200; body=%q", w.Code, w.Body.String())
			}
		})
	}
}

func TestOwnerOnlyMutationsRejectSpoofedOwnerRoleWithWrongToken(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed owner role with wrong token status = %d, want 401", w.Code)
	}
}

// TestOwnerOnlyMutationsRejectSpoofedOwnerVerificationMarker: the server-only
// verification marker is stripped from inbound requests, so spoofing it (with
// role headers, without any valid credential) gains nothing.
func TestOwnerOnlyMutationsRejectSpoofedOwnerVerificationMarker(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set("X-Hive-Owner-Role-Verified", "true")
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed owner verification marker without credential status = %d, want 401", w.Code)
	}
}

// TestOwnerOnlyMutationsAllowInternalSharedToken pins the #4134 fix for the
// gateway path: the local proxy authenticates the browser, strips client
// identity headers, and injects X-Hive-Internal — that request is the
// operator and must pass owner-gated endpoints.
func TestOwnerOnlyMutationsAllowInternalSharedToken(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-Internal", s.authToken)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("internal shared token owner status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// A per-user session always scopes an internal-token request DOWN: a read-role
// session riding alongside X-Hive-Internal must not inherit owner.
func TestOwnerOnlyMutationsRejectReadSessionWithInternalAuth(t *testing.T) {
	s := newDirectRouteServer(t, "readuser")
	sid := s.createUserSession("readuser", "read")
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-Internal", s.authToken)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("read session with internal auth status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

func TestOwnerOnlyMutationsAllowOwnerSessionWithInternalAuth(t *testing.T) {
	s := newDirectRouteServer(t, "owneruser")
	sid := s.createUserSession("owneruser", "owner")
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-Internal", s.authToken)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("owner session with internal auth status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

func TestOwnerOnlyMutationsAllowVerifiedHubOwnerRole(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-User", "owner")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, s.authToken)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("verified hub owner role status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

func TestOwnerOnlyMutationsRejectProoflessHubOwnerRole(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-User", "owner")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("proofless hub owner role status = %d, want 401", w.Code)
	}
}

func TestOwnerOnlyMutationsPreserveOpenSpokeOwnerBehavior(t *testing.T) {
	s := newFullServer(t)
	s.authToken = ""
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("open spoke owner behavior status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

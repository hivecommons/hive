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

func TestOwnerOnlyMutationsRejectSharedTokenWithoutRole(t *testing.T) {
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

			if w.Code != http.StatusForbidden {
				t.Fatalf("shared-token request without server role status = %d, want 403", w.Code)
			}
		})
	}
}

func TestOwnerOnlyMutationsRejectSpoofedOwnerRoleWithSharedToken(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed owner role with shared token status = %d, want 403", w.Code)
	}
}

func TestOwnerOnlyMutationsRejectSpoofedOwnerVerificationMarker(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set("X-Hive-Owner-Role-Verified", "true")
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed owner verification marker with shared token status = %d, want 403", w.Code)
	}
}

func TestOwnerOnlyMutationsRejectInternalSharedTokenWithoutVerifiedRole(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest(http.MethodPost, "/api/breaker/engage", nil)
	req.Header.Set("X-Hive-Internal", s.authToken)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("internal shared token without verified role status = %d, want 403", w.Code)
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

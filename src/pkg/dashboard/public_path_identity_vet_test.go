package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// Tests for vetPublicPathIdentity (#5771): public paths bypass authenticate's
// auth branches, but client-forged X-Hive-User / X-Hive-Role must never reach
// public handlers that treat those headers as hub-injected identity
// (requireContributorWrite on the queue controls, resolveViewerUsername, the
// gh-user-auth status echo). Identity survives ONLY with a valid
// X-Hive-Proxy-Auth proof (or the legacy no-proof rollout window).

func newVetTestServer(t *testing.T) *Server {
	t.Helper()
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	return s
}

// echoIdentity registers a public-path probe that reports the identity headers
// exactly as a public handler would observe them after the middleware chain.
func serveVetProbe(s *Server, r *http.Request) (user, role, verified string) {
	var gotUser, gotRole, gotVerified string
	s.mux.HandleFunc("GET /api/contribute/vet-probe", func(w http.ResponseWriter, req *http.Request) {
		gotUser = req.Header.Get("X-Hive-User")
		gotRole = req.Header.Get("X-Hive-Role")
		gotVerified = req.Header.Get(ownerRoleVerifiedHeader)
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return gotUser, gotRole, gotVerified
}

func TestVetPublicPath_ForgedIdentityStripped(t *testing.T) {
	s := newVetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/vet-probe", nil)
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	user, role, verified := serveVetProbe(s, req)
	if user != "" || role != "" || verified != "" {
		t.Fatalf("forged identity must be stripped on public paths, got user=%q role=%q verified=%q", user, role, verified)
	}
}

func TestVetPublicPath_HubProofedIdentityKept(t *testing.T) {
	s := newVetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/vet-probe", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(proxyAuthHeader, "shared-secret-token")
	user, role, verified := serveVetProbe(s, req)
	if user != "clubanderson" || role != config.RoleOwner || verified != "true" {
		t.Fatalf("hub-proofed identity must survive, got user=%q role=%q verified=%q", user, role, verified)
	}
}

func TestVetPublicPath_WrongProofStripped(t *testing.T) {
	s := newVetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/vet-probe", nil)
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(proxyAuthHeader, "wrong-proof")
	user, role, _ := serveVetProbe(s, req)
	if user != "" || role != "" {
		t.Fatalf("identity with an invalid proof must be stripped, got user=%q role=%q", user, role)
	}
}

func TestVetPublicPath_DirectRouteNeverTrustsHubHeaders(t *testing.T) {
	s := newVetTestServer(t)
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson"}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/vet-probe", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(proxyAuthHeader, "shared-secret-token")
	user, role, _ := serveVetProbe(s, req)
	if user != "" || role != "" {
		t.Fatalf("direct-route spokes must not trust hub identity headers, got user=%q role=%q", user, role)
	}
}

func TestVetPublicPath_LegacyNoProofWindowKeepsIdentityButNotOwnerVerified(t *testing.T) {
	s := newVetTestServer(t)
	orig := proxyProofRequired
	proxyProofRequired = false
	defer func() { proxyProofRequired = orig }()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/vet-probe", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	user, role, verified := serveVetProbe(s, req)
	if user != "clubanderson" || role != config.RoleOwner {
		t.Fatalf("legacy no-proof window must keep identity, got user=%q role=%q", user, role)
	}
	if verified != "" {
		t.Fatalf("legacy no-proof window must NOT mark owner verified, got %q", verified)
	}
}

// End-to-end regression for the reported exploit: an anonymous caller forging
// X-Hive-Role: owner must NOT be able to park the ready-work queue or clear the
// operator hold set through the public /api/contribute prefix — while the same
// request carrying the hub proof still works.
func TestQueueControls_ForgedOwnerRoleDenied_ProofedOwnerAllowed(t *testing.T) {
	setupContributeEnv(t)
	s := newVetTestServer(t)
	h := s.Handler()

	mutations := []struct {
		name, method, path, body string
	}{
		{"hold", http.MethodPost, "/api/contribute/queue/hold", `{"key":"kubestellar/hive#1","held":true}`},
		{"hold-clear", http.MethodPost, "/api/contribute/queue/hold/clear", ``},
		{"order", http.MethodPut, "/api/contribute/queue/order", `{"order":["kubestellar/hive#1"]}`},
	}
	for _, m := range mutations {
		t.Run("forged_"+m.name, func(t *testing.T) {
			req := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
			req.Header.Set("X-Hive-Role", config.RoleOwner)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("forged owner %s %s got %d, want 403 (body: %s)", m.method, m.path, rec.Code, rec.Body.String())
			}
		})
	}
	if len(s.deps.Config.Hub.ContributeQueueHold) != 0 || len(s.deps.Config.Hub.ContributeQueueOrder) != 0 {
		t.Fatalf("forged requests mutated queue state: hold=%v order=%v",
			s.deps.Config.Hub.ContributeQueueHold, s.deps.Config.Hub.ContributeQueueOrder)
	}

	// The legitimate hub-proxied owner (identity + proof) is untouched.
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/queue/hold", strings.NewReader(`{"key":"kubestellar/hive#1","held":true}`))
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	req.Header.Set(proxyAuthHeader, "shared-secret-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hub-proofed owner queue hold got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(s.deps.Config.Hub.ContributeQueueHold) != 1 {
		t.Fatalf("proofed hold did not persist: %v", s.deps.Config.Hub.ContributeQueueHold)
	}
}

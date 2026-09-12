package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Coverage for the sessionless header-fallback lane of
// requestHasGitHubSetupAdmin (api.go). The session lane is pinned by
// live_role_residual_4299_test.go; these tests pin the three remaining
// branches: the direct-route short-circuit, and the proxy-proof compare with
// its fail-closed cases (empty token, missing proof, wrong proof).

// fallbackReq builds a sessionless request carrying the given identity
// headers, exactly what the hub nginx lane injects.
func fallbackReq(role, proof string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	}
	return req
}

// On the hub-proxied lane (no direct-route allowlist), a read-write role with
// the correct proxy proof confers GitHub-setup admin — and every degraded
// variant of that credential pair must fail closed.
func TestGitHubSetupAdminFallback_ProxyProofLane(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"

	if !s.requestHasGitHubSetupAdmin(fallbackReq("owner", "shared-secret-token")) {
		t.Error("owner role with the correct proxy proof must confer GitHub-setup admin")
	}
	if !s.requestHasGitHubSetupAdmin(fallbackReq("read-write", "shared-secret-token")) {
		t.Error("read-write role with the correct proxy proof must confer GitHub-setup admin")
	}
	if s.requestHasGitHubSetupAdmin(fallbackReq("read", "shared-secret-token")) {
		t.Error("a read role must never confer GitHub-setup admin, even with a valid proof")
	}
	if s.requestHasGitHubSetupAdmin(fallbackReq("owner", "wrong-proof")) {
		t.Error("a wrong proxy proof must fail closed")
	}
	if s.requestHasGitHubSetupAdmin(fallbackReq("owner", "")) {
		t.Error("a missing proxy proof must fail closed")
	}
	if s.requestHasGitHubSetupAdmin(fallbackReq("", "shared-secret-token")) {
		t.Error("a missing role header must fail closed regardless of proof")
	}
}

// An empty authToken means there is no shared secret to compare against;
// secureCompare("","") must not be reachable as a grant.
func TestGitHubSetupAdminFallback_EmptyTokenFailsClosed(t *testing.T) {
	s := newFullServer(t)
	s.authToken = ""

	if s.requestHasGitHubSetupAdmin(fallbackReq("owner", "")) {
		t.Error("empty authToken with empty proof must not confer GitHub-setup admin")
	}
	if s.requestHasGitHubSetupAdmin(fallbackReq("owner", "anything")) {
		t.Error("empty authToken must never confer GitHub-setup admin")
	}
}

// On a direct-route spoke (per-user allowlist configured) client-supplied
// identity headers are untrusted: the fallback lane must refuse even a
// correct proxy proof, because identity comes only from a session.
func TestGitHubSetupAdminFallback_DirectRouteShortCircuits(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner")

	if s.requestHasGitHubSetupAdmin(fallbackReq("owner", "shared-secret-token")) {
		t.Error("direct-route spoke must ignore header identity even with a valid proxy proof")
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Follow-up regression tests for #4299: after the middleware fix (#4302),
// four handlers still re-read the RAW session and preferred the role frozen
// at login over the live allowlist — handleRole and handleGHUserAuthStatus
// (what the UI believes the user may do), requestHasGitHubSetupAdmin (a real
// authz decision), and handleBannerDismissed. Daniel's second symptom
// ("Failed to save advisory: owner access required" while the hive CREATOR
// passed) is the same class: the creator's session was minted as owner, the
// grantee's session predates the grant. These tests pin the advisory endpoint
// and the residual handlers to the live allowlist.

func do4299(s *Server, method, target, body, sid string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// handleRole drives the UI's idea of the user's capabilities; it must report
// the live allowlist role, not the one frozen into the session.
func TestRoleEndpoint4299_ReportsLiveRole(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:read-write")
	sid := s.createUserSession("dwaddington", "read-write")
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:owner"}

	w := do4299(s, http.MethodGet, "/api/role", "", sid)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/role = %d; body=%q", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["role"] != "owner" {
		t.Fatalf("role = %q, want owner (live grant, not frozen session role)", resp["role"])
	}
}

// handleGHUserAuthStatus must report the live role, and a revoked session must
// read logged-out instead of a ghost identity.
func TestGHUserAuthStatus4299_LiveRoleAndRevocation(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:read")
	sid := s.createUserSession("dwaddington", "read")
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:owner"}

	w := do4299(s, http.MethodGet, "/api/gh-user-auth/status", "", sid)
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("status %d, body %q: %v", w.Code, w.Body.String(), err)
	}
	if resp["role"] != "owner" {
		t.Fatalf("auth-status role = %v, want owner", resp["role"])
	}

	// Revoke: the same session must no longer present an identity.
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}
	w = do4299(s, http.MethodGet, "/api/gh-user-auth/status", "", sid)
	// The authenticate middleware already rejects the revoked session with 401
	// before the handler runs; either that or a logged_in=false body is
	// acceptable — what must NOT happen is logged_in=true.
	if w.Code == http.StatusOK {
		var revoked map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &revoked); err != nil {
			t.Fatal(err)
		}
		if loggedIn, _ := revoked["logged_in"].(bool); loggedIn {
			t.Fatalf("revoked session still reports logged_in=true: %q", w.Body.String())
		}
	} else if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked auth-status = %d, want 200 logged_out or 401; body=%q", w.Code, w.Body.String())
	}
}

// requestHasGitHubSetupAdmin is a real authz decision: a live downgrade to
// read must strip it, and a live grant must confer it, on a stale session.
func TestGitHubSetupAdmin4299_FollowsLiveRole(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson:owner", "dwaddington:read")
	sid := s.createUserSession("dwaddington", "read")
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:owner"}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	if !s.requestHasGitHubSetupAdmin(req) {
		t.Fatal("live owner grant must confer GitHub-setup admin on a stale read session")
	}

	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "dwaddington:read"}
	if s.requestHasGitHubSetupAdmin(req) {
		t.Fatal("live downgrade to read must strip GitHub-setup admin despite an owner-minted session")
	}

	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner"}
	if s.requestHasGitHubSetupAdmin(req) {
		t.Fatal("revoked user must not keep GitHub-setup admin from a stale session")
	}
}

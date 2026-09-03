package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// --- config.AuthorizedRole ---

func TestAuthorizedRole_OwnerFirstEntryIsWrite(t *testing.T) {
	d := config.DashboardConfig{AuthorizedUsers: []string{"clubanderson", "kellyaa"}}
	role, ok := d.AuthorizedRole("clubanderson")
	if !ok || role != config.RoleOwner {
		t.Fatalf("expected owner for first entry, got role=%q ok=%v", role, ok)
	}
}

func TestAuthorizedRole_NonOwnerDefaultsToRead(t *testing.T) {
	d := config.DashboardConfig{AuthorizedUsers: []string{"clubanderson", "kellyaa"}}
	role, ok := d.AuthorizedRole("kellyaa")
	if !ok || role != config.RoleRead {
		t.Fatalf("expected read for non-first entry, got role=%q ok=%v", role, ok)
	}
}

func TestAuthorizedRole_CaseInsensitive(t *testing.T) {
	d := config.DashboardConfig{AuthorizedUsers: []string{"ClubAnderson"}}
	if role, ok := d.AuthorizedRole("clubanderson"); !ok || role != config.RoleOwner {
		t.Fatalf("expected case-insensitive owner match, got role=%q ok=%v", role, ok)
	}
}

func TestAuthorizedRole_ExplicitRoleSuffix(t *testing.T) {
	d := config.DashboardConfig{AuthorizedUsers: []string{"owneruser:owner", "vieweruser:read"}}
	if role, ok := d.AuthorizedRole("vieweruser"); !ok || role != config.RoleRead {
		t.Fatalf("expected explicit read role, got role=%q ok=%v", role, ok)
	}
	if role, ok := d.AuthorizedRole("owneruser"); !ok || role != config.RoleOwner {
		t.Fatalf("expected explicit owner role, got role=%q ok=%v", role, ok)
	}
}

func TestAuthorizedRole_UnauthorizedUserRejected(t *testing.T) {
	d := config.DashboardConfig{AuthorizedUsers: []string{"clubanderson"}}
	if _, ok := d.AuthorizedRole("attacker"); ok {
		t.Fatal("unauthorized user must not resolve to a role")
	}
}

func TestAuthorizedRole_EmptyListNotEnabled(t *testing.T) {
	d := config.DashboardConfig{}
	if d.IsDirectRouteAuthzEnabled() {
		t.Fatal("empty allowlist must not enable direct-route authz")
	}
	if _, ok := d.AuthorizedRole("anyone"); ok {
		t.Fatal("empty allowlist must authorize nobody")
	}
}

// --- per-user session store ---

func TestUserSession_DistinctPerUser(t *testing.T) {
	s := &Server{userSessions: map[string]*userSession{}}
	idA := s.createUserSession("alice", config.RoleOwner)
	idB := s.createUserSession("bob", config.RoleRead)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("expected two distinct non-empty session ids, got %q and %q", idA, idB)
	}
	a := s.lookupSession(idA)
	b := s.lookupSession(idB)
	if a == nil || a.Username != "alice" || a.Role != config.RoleOwner {
		t.Fatalf("session A resolved wrong user: %+v", a)
	}
	if b == nil || b.Username != "bob" || b.Role != config.RoleRead {
		t.Fatalf("session B resolved wrong user: %+v", b)
	}
}

func TestUserSession_LogoutClearsOnlyThatSession(t *testing.T) {
	s := &Server{userSessions: map[string]*userSession{}}
	idA := s.createUserSession("alice", config.RoleOwner)
	idB := s.createUserSession("bob", config.RoleRead)
	s.deleteSession(idA)
	if s.lookupSession(idA) != nil {
		t.Fatal("deleted session must not resolve")
	}
	if s.lookupSession(idB) == nil {
		t.Fatal("other user's session must survive logout")
	}
}

func TestUserSession_UnknownIDResolvesNil(t *testing.T) {
	s := &Server{userSessions: map[string]*userSession{}}
	if s.lookupSession("does-not-exist") != nil {
		t.Fatal("unknown session id must resolve to nil")
	}
}

// --- middleware: direct-route strips forged identity headers ---

func newDirectRouteServer(t *testing.T, authorized ...string) *Server {
	t.Helper()
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	s.deps.Config.Dashboard.AuthorizedUsers = authorized
	return s
}

// okHandler is a terminal handler that records the identity headers it sees.
func recordingHandler(sawUser, sawRole *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sawUser != nil {
			*sawUser = r.Header.Get("X-Hive-User")
		}
		if sawRole != nil {
			*sawRole = r.Header.Get("X-Hive-Role")
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddleware_DirectRouteStripsForgedHeaders(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")
	var sawUser string
	handler := s.authenticate(recordingHandler(&sawUser, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("X-Hive-User", "kellyaa")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	// No valid session/token → request must be rejected, not trusted.
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("forged headers on direct route must be unauthorized, got %d", w.Code)
	}
	if sawUser != "" {
		t.Errorf("forged X-Hive-User must not reach the handler: %q", sawUser)
	}
}

func TestMiddleware_DirectRouteSessionResolvesUser(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")
	sid := s.createUserSession("clubanderson", config.RoleOwner)
	var sawUser, sawRole string
	handler := s.authenticate(recordingHandler(&sawUser, &sawRole))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid session must be authorized, got %d", w.Code)
	}
	if sawUser != "clubanderson" || sawRole != config.RoleOwner {
		t.Fatalf("session must inject its own identity, got user=%q role=%q", sawUser, sawRole)
	}
}

func TestHandleRoleIgnoresLeakedHubCookie(t *testing.T) {
	s := newDirectRouteServer(t, "realuser:read")
	sid := s.createUserSession("realuser", config.RoleRead)
	req := httptest.NewRequest("GET", "/api/role", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "castrojo"})
	w := httptest.NewRecorder()

	s.handleRole(w, req)

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/role: %v; body=%q", err, w.Body.String())
	}
	if got["user"] != "realuser" {
		t.Fatalf("/api/role user = %q, want realuser (must not use hive_hub_user)", got["user"])
	}
	if got["role"] != config.RoleRead {
		t.Fatalf("/api/role role = %q, want read", got["role"])
	}
}

func TestMiddleware_HubProxiedHeadersStillTrusted(t *testing.T) {
	// Empty allowlist = hub-proxied hive; nginx-injected headers must still work.
	// F7: since proxyProofRequired now defaults to true (fail closed), the
	// legitimate hub path carries the X-Hive-Proxy-Auth proof (the hub's
	// auth-check sets it fleet-wide). With a valid proof the identity headers are
	// trusted; without it the request would be rejected (see the no-proof case
	// below and TestMiddleware_F2ProxyProof).
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	var sawUser string
	handler := s.authenticate(recordingHandler(&sawUser, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, "shared-secret-token") // proof the hub injects
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("hub-proxied request with valid proof must be authorized, got %d", w.Code)
	}
	if sawUser != "clubanderson" {
		t.Errorf("hub-proxied identity must be preserved, got %q", sawUser)
	}

	// Same identity headers WITHOUT the proof: now rejected by default (F7). This
	// is the forged direct-to-:3002 request the fail-closed default blocks.
	reqNoProof := httptest.NewRequest("GET", "/api/config", nil)
	reqNoProof.Header.Set("X-Hive-User", "clubanderson")
	reqNoProof.Header.Set("X-Hive-Role", "owner")
	wNoProof := httptest.NewRecorder()
	s.authenticate(recordingHandler(nil, nil)).ServeHTTP(wNoProof, reqNoProof)
	if wNoProof.Code == http.StatusOK {
		t.Fatal("forged identity headers with no proof must be rejected under the fail-closed default (F7)")
	}
}

// TestMiddleware_F2ProxyProof covers the F2 proof-of-proxy header enforcement on
// a hub-proxied spoke: a valid proof is trusted; a WRONG proof is always
// rejected; a MISSING proof is trusted only in the fail-open rollout default and
// rejected once strict enforcement is enabled.
func TestMiddleware_F2ProxyProof(t *testing.T) {
	newReq := func(proof string) *http.Request {
		r := httptest.NewRequest("GET", "/api/config", nil)
		r.Header.Set("X-Hive-User", "clubanderson")
		r.Header.Set("X-Hive-Role", "owner")
		if proof != "" {
			r.Header.Set(proxyAuthHeader, proof)
		}
		return r
	}
	run := func(t *testing.T, strict bool, proof string) int {
		s := newFullServer(t)
		s.authToken = "shared-secret-token"
		old := proxyProofRequired
		proxyProofRequired = strict
		t.Cleanup(func() { proxyProofRequired = old })
		w := httptest.NewRecorder()
		s.authenticate(recordingHandler(nil, nil)).ServeHTTP(w, newReq(proof))
		return w.Code
	}

	// Valid proof header (matches authToken) -> trusted, both modes.
	if c := run(t, false, "shared-secret-token"); c != http.StatusOK {
		t.Errorf("valid proof (fail-open) = %d, want 200", c)
	}
	if c := run(t, true, "shared-secret-token"); c != http.StatusOK {
		t.Errorf("valid proof (strict) = %d, want 200", c)
	}
	// Wrong proof header -> ALWAYS rejected (forged proxy).
	if c := run(t, false, "wrong-token"); c == http.StatusOK {
		t.Error("wrong proof must be rejected even in fail-open mode")
	}
	if c := run(t, true, "wrong-token"); c == http.StatusOK {
		t.Error("wrong proof must be rejected in strict mode")
	}
	// Missing proof: trusted in fail-open (rollout), rejected in strict.
	if c := run(t, false, ""); c != http.StatusOK {
		t.Errorf("missing proof (fail-open) = %d, want 200 (rollout safety)", c)
	}
	if c := run(t, true, ""); c == http.StatusOK {
		t.Error("missing proof must be rejected in strict mode")
	}
}

func TestMiddleware_F2ProxyProofDefaultRejectsMissingProof(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	handler := s.authenticate(recordingHandler(nil, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("X-Hive-User", "clubanderson")
	req.Header.Set("X-Hive-Role", "owner")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing proof with default enforcement = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing proxy proof header") {
		t.Fatalf("missing proof error = %q, want clear proxy proof error", w.Body.String())
	}
}

func TestMiddleware_SharedCookieNoLongerAuthenticates(t *testing.T) {
	// The old vulnerability: the shared s.authToken placed in the session cookie
	// granted access. It must no longer be accepted as a session id.
	s := newDirectRouteServer(t, "clubanderson")
	handler := s.authenticate(recordingHandler(nil, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.authToken})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("shared auth token in session cookie must NOT authenticate")
	}
}

func TestMiddleware_BearerTokenDisabledOnDirectRoute(t *testing.T) {
	// The shared token is publicly discoverable and carries no per-user
	// identity; it must NOT grant access on a direct-route spoke, else it
	// defeats the per-hive allowlist.
	s := newDirectRouteServer(t, "clubanderson")
	handler := s.authenticate(recordingHandler(nil, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("shared bearer token must NOT authenticate on a direct-route spoke")
	}
}

func TestMiddleware_BearerTokenWorksWhenNoAllowlist(t *testing.T) {
	// Self-hosted single-user path (no allowlist): the shared bearer token is
	// how the browser fetch wrapper authenticates and must still work.
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	handler := s.authenticate(recordingHandler(nil, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+s.authToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bearer shared-token API access must still work without allowlist, got %d", w.Code)
	}
}

func TestMiddleware_InternalHeaderTrustedOnDirectRoute(t *testing.T) {
	// The local proxy authenticates server-to-server with X-Hive-Internal; this
	// must still work even on a direct-route spoke.
	s := newDirectRouteServer(t, "clubanderson")
	handler := s.authenticate(recordingHandler(nil, nil))
	req := httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("X-Hive-Internal", s.authToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("X-Hive-Internal must authenticate on a direct-route spoke, got %d", w.Code)
	}
}

// TestFullChain_ReadViewerCannotWrite exercises the REAL middleware order
// (authenticate → roleEnforcement) to prove a read-only session is blocked on a
// mutating request. If authenticate ran INSIDE roleEnforcement, roleEnforcement
// would read an empty role and let the write through.
func TestFullChain_ReadViewerCannotWrite(t *testing.T) {
	s := newDirectRouteServer(t, "owneruser", "vieweruser:read")
	sid := s.createUserSession("vieweruser", config.RoleRead)

	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	handler := s.authenticate(s.roleEnforcement(inner))

	req := httptest.NewRequest("POST", "/api/pause/scanner", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("read-only viewer POST must be 403, got %d", w.Code)
	}
	if reached {
		t.Fatal("read-only viewer's write reached the handler — read-only gate bypassed")
	}
}

func TestFullChain_OwnerCanWrite(t *testing.T) {
	s := newDirectRouteServer(t, "owneruser")
	sid := s.createUserSession("owneruser", config.RoleOwner)

	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	handler := s.authenticate(s.roleEnforcement(inner))

	req := httptest.NewRequest("POST", "/api/pause/scanner", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !reached || w.Code != http.StatusOK {
		t.Fatalf("owner POST must reach handler with 200, got code=%d reached=%v", w.Code, reached)
	}
}

// --- status endpoint reflects the CURRENT session, not the persisted token ---

func TestHandleGHUserAuthStatus_DirectRouteNoSessionIsLoggedOut(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	w := httptest.NewRecorder()
	s.handleGHUserAuthStatus(w, req)
	if got := w.Body.String(); !strings.Contains(got, `"logged_in":false`) {
		t.Fatalf("direct-route status without session must be logged_in:false, got %s", got)
	}
}

func TestHandleGHUserAuthStatus_ReflectsSessionUser(t *testing.T) {
	s := newDirectRouteServer(t, "clubanderson")
	sid := s.createUserSession("clubanderson", config.RoleOwner)
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	s.handleGHUserAuthStatus(w, req)
	got := w.Body.String()
	if !strings.Contains(got, `"logged_in":true`) || !strings.Contains(got, `"clubanderson"`) {
		t.Fatalf("status must reflect the session user, got %s", got)
	}
}

func TestHandleGHUserAuthStatus_HubProxiedReflectsHeaderUser(t *testing.T) {
	// Hub-proxied hive: report the nginx-injected user, not the shared token.
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest("GET", "/api/gh-user-auth/status", nil)
	req.Header.Set("X-Hive-User", "kellyaa")
	req.Header.Set("X-Hive-Role", "read")
	w := httptest.NewRecorder()
	s.handleGHUserAuthStatus(w, req)
	got := w.Body.String()
	if !strings.Contains(got, `"logged_in":true`) || !strings.Contains(got, `"kellyaa"`) {
		t.Fatalf("hub-proxied status must reflect the header user, got %s", got)
	}
}

func TestHandleAuthToken_HiddenOnDirectRoute(t *testing.T) {
	// The shared token must never be exposed to a browser on a direct-route hive.
	s := newDirectRouteServer(t, "clubanderson")
	req := httptest.NewRequest("GET", "/api/auth/token", nil)
	w := httptest.NewRecorder()
	s.handleAuthToken(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("auth token endpoint must 404 on direct-route spoke, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), s.authToken) {
		t.Fatal("shared token must never be returned on a direct-route spoke")
	}
}

func TestHandleAuthToken_HiddenOnHubProxied(t *testing.T) {
	// On a hub-proxied spoke identity comes from nginx-injected headers; the
	// shared token is server-to-server only. Reporting configured=true here
	// made the SPA prompt hosted operators to paste a token they were never
	// given (and a leaked value would let read-role users escalate to writes).
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	s.deps.Config.Dashboard.HubProxied = true
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"clubanderson:owner", "someone:read"}
	req := httptest.NewRequest("GET", "/api/auth/token", nil)
	w := httptest.NewRecorder()
	s.handleAuthToken(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("auth token endpoint must 404 on hub-proxied spoke, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), s.authToken) {
		t.Fatal("shared token must never be returned on a hub-proxied spoke")
	}
}

func TestHandleAuthToken_AvailableWithoutAllowlist(t *testing.T) {
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	req := httptest.NewRequest("GET", "/api/auth/token", nil)
	w := httptest.NewRecorder()
	s.handleAuthToken(w, req)
	// On a non-allowlist (self-hosted) hive the endpoint reports that a token is
	// configured but must NEVER return its value (F1): returning it let any
	// visitor read the API credential.
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got code=%d", w.Code)
	}
	if strings.Contains(w.Body.String(), s.authToken) {
		t.Fatalf("auth-token endpoint leaked the token value: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"configured":"true"`) {
		t.Fatalf("expected configured:true, got %s", w.Body.String())
	}
}

package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/hub"
)

func dfLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// deviceFlowMock serves the GitHub device-flow endpoints (device/code,
// access_token, /user) so handleGHUserAuthStart/Poll and ValidateToken can be
// driven end-to-end without real network access. tokenStatus controls the
// access_token response ("complete", "authorization_pending", "slow_down",
// "error").
func deviceFlowMock(t *testing.T, tokenStatus, login string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev-code-123",
			"user_code":        "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900,
			"interval":         5,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch tokenStatus {
		case "complete":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_validtoken", "token_type": "bearer"})
		case "authorization_pending":
			json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
		case "slow_down":
			json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"error": "access_denied", "error_description": "denied"})
		}
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if login == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"login": login, "avatar_url": "https://example/a.png"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// dfServer builds a Server whose GitHub endpoints point at the device-flow mock
// and returns it along with the deps for further tweaking.
func dfServer(t *testing.T, tokenStatus, login string) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	t.Setenv("HIVE_SSO_PUBLIC_KEY", "")
	t.Setenv("HIVE_SSO_PUBLIC_KEY_PREV", "")
	mock := deviceFlowMock(t, tokenStatus, login)
	s := NewServerWithAuth(0, "authsecret", dfLogger())
	deps := testDeps(t)
	deps.Config.GitHub.OAuthClientID = "Ov23liTest"
	deps.Config.GitHub.BaseURL = mock.URL
	deps.Config.GitHub.APIURL = mock.URL
	// Device-flow login resolves through OAuthBaseURL/OAuthAPIURL, which are
	// pinned to public github.com for GHE hives. Without these overrides the
	// handlers ignore the mock and POST to the real github.com.
	deps.Config.GitHub.OAuthBaseURLOverride = mock.URL
	deps.Config.GitHub.OAuthAPIURLOverride = mock.URL
	s.RegisterAPI(deps)
	return s, deps, mock
}

// A blank oauth_client_id is NOT an error: OAuthClientIDResolved() falls back to
// the public github.com Hive App client, which is the correct client for every
// hive including GHE ones. This asserts the fallback actually reaches the device
// flow rather than short-circuiting to 400 as an earlier revision did.
func TestCovDF_GHUserAuthStart_BlankClientIDUsesPublicDefault(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.GitHub.OAuthClientID = ""
	rec := doPost(s, "/api/gh-user-auth/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("blank client id: want 200 via public default, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_code"] != "ABCD-1234" {
		t.Fatalf("device flow did not run: user_code=%v", body["user_code"])
	}
}

func TestCovDF_GHUserAuthStart_OK(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doPost(s, "/api/gh-user-auth/start", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["user_code"] != "ABCD-1234" {
		t.Fatalf("unexpected user_code: %v", body["user_code"])
	}
}

func TestCovDF_GHUserAuthPoll_NoFlow(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	if rec := doPost(s, "/api/gh-user-auth/poll", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no flow: want 400, got %d", rec.Code)
	}
}

// TestCovDF_GHUserAuthPoll_MissingFlowIDRejected: a poll that presents no
// flow_id at all (an anonymous prober, or a pre-binding client) must be
// rejected and must never complete a login.
func TestCovDF_GHUserAuthPoll_MissingFlowIDRejected(t *testing.T) {
	s, _, _ := dfServer(t, "authorization_pending", "octocat")
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("poll without flow_id: want 400, got %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] == "complete" {
		t.Fatal("poll without flow_id must never complete a login")
	}
}

// TestCovDF_GHUserAuthPoll_WrongFlowIDRejected pins the session-hijack fix: the
// session cookie is minted on the POLL response and both routes are public, so
// a poll that cannot present the flow_id returned by ITS OWN start must get
// nothing — otherwise any anonymous poller could race the operator and steal
// the freshly approved session.
func TestCovDF_GHUserAuthPoll_WrongFlowIDRejected(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", map[string]string{"flow_id": "attacker-guess"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("poll with wrong flow_id: want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if c := sessionCookie(rec); c != nil {
		t.Fatal("session cookie minted for a poll that did not start the flow")
	}
	// The legitimate holder's flow must survive the attacker's attempt.
	flow := map[string]string{}
	s.deviceFlowMu.Lock()
	if s.deviceFlowState == nil {
		s.deviceFlowMu.Unlock()
		t.Fatal("a rejected poll must not destroy the in-progress flow")
	}
	flow["flow_id"] = s.deviceFlowID
	s.deviceFlowMu.Unlock()
	rec = doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "complete" && body["status"] != "error" {
		t.Fatalf("legitimate poll after rejected one: got %v (%s)", body["status"], rec.Body.String())
	}
}

// dfStartFlow starts a device flow through the public handler and returns the
// poll body carrying the flow_id the server now requires (the client binding
// that prevents the poll-race session hijack).
func dfStartFlow(t *testing.T, s *Server) map[string]string {
	t.Helper()
	rec := doPost(s, "/api/gh-user-auth/start", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	id, _ := body["flow_id"].(string)
	if id == "" {
		t.Fatalf("start response carried no flow_id: %s", rec.Body.String())
	}
	return map[string]string{"flow_id": id}
}

func TestCovDF_GHUserAuthPoll_Pending(t *testing.T) {
	s, _, _ := dfServer(t, "authorization_pending", "octocat")
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "pending" {
		t.Fatalf("want pending, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_SlowDown(t *testing.T) {
	s, _, _ := dfServer(t, "slow_down", "octocat")
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "slow_down" {
		t.Fatalf("want slow_down, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_Error(t *testing.T) {
	s, _, _ := dfServer(t, "error", "octocat")
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "error" {
		t.Fatalf("want error, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_CompleteOwner(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	// Not direct-route (no allowlist) → role owner, token persisted only if
	// userTokenPath is writable.
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	// Either "complete" (if /data writable) or an error about persisting token —
	// both exercise the owner-role branch. In CI /data is absent so we expect the
	// persist-error path, which still covers the code.
	if body["status"] != "complete" && body["status"] != "error" {
		t.Fatalf("unexpected status %v (body=%s)", body["status"], rec.Body.String())
	}
}

func TestCovDF_GHUserAuthPoll_UnverifiableIdentity(t *testing.T) {
	// login empty → /user returns 401 → ValidateToken fails → identity error.
	s, _, _ := dfServer(t, "complete", "")
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "error" {
		t.Fatalf("want error for unverifiable identity, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_DirectRouteDenied(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	// Direct-route allowlist that does NOT include octocat → denied.
	deps.Config.Dashboard.AuthorizedUsers = []string{"someoneelse"}
	deps.Config.Dashboard.HubProxied = false
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "error" {
		t.Fatalf("want error (denied), got %v", body["status"])
	}
	if !strings.Contains(body["error"].(string), "not authorized") {
		t.Fatalf("expected not-authorized message, got %v", body["error"])
	}
}

func TestCovDF_GHUserAuthPoll_DirectRouteViewer(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "viewer1")
	// Allowlist with owner first, viewer as read-only → viewer authorized but
	// role read (no token persistence).
	deps.Config.Dashboard.AuthorizedUsers = []string{"owner1:owner", "viewer1:read"}
	deps.Config.Dashboard.HubProxied = false
	flow := dfStartFlow(t, s)
	rec := doPost(s, "/api/gh-user-auth/poll", flow)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "complete" {
		t.Fatalf("want complete for authorized viewer, got %v (%s)", body["status"], rec.Body.String())
	}
	if body["username"] != "viewer1" {
		t.Fatalf("want viewer1, got %v", body["username"])
	}
}

// TestCovDF_GHUserAuthPoll_EmptyAuthTokenStillMintsSession is the device-flow
// twin of the SSO regression test: minting the session used to be gated on
// s.authToken != "", so a spoke provisioned without a dashboard token returned
// {status:"complete"} having set NO cookie at all — "/" rejected the very next
// request and the login page bounced forever. The session store is the
// authority on identity and does not depend on a shared token existing, so the
// cookie must be set even when authToken is empty. The C3 terminal-assertion
// cookie is minted in the same path and must not be lost either.
func TestCovDF_GHUserAuthPoll_EmptyAuthTokenStillMintsSession(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	s.authToken = ""
	// Give the terminal-assertion mint a signing key and a hive identity so the
	// C3 cookie is observable (it is a no-op without both).
	//
	// HIVE_ID is required as well as the master: TerminalSigningKey's fallback
	// lane now SELF-DERIVES the PER-HIVE key (audit N3) rather than deriving a
	// fleet-uniform one, so a master alone no longer resolves to any key.
	deps.Config.HiveID = testHiveID
	t.Setenv("HIVE_HUB_SECRET", testHubSecret)
	t.Setenv("HIVE_ID", testHiveID)

	flow := dfStartFlow(t, s)

	// Drive the poll with X-Forwarded-Proto set, mirroring production traffic
	// behind the TLS-terminating proxy, so Secure cookie attributes are asserted.
	flowBody, _ := json.Marshal(flow)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/poll", bytes.NewReader(flowBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	s.mux.ServeHTTP(rec, req)

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "complete" {
		t.Fatalf("want complete, got %v (%s)", body["status"], rec.Body.String())
	}

	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no hive_session cookie set with empty authToken — the login page would bounce forever")
	}
	if c.Value == "" {
		t.Fatal("hive_session cookie set to an empty value")
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if !c.Secure {
		t.Error("session cookie must be Secure behind TLS-terminating nginx")
	}
	if c.Path != "/" {
		t.Errorf("cookie Path = %q, want / so it is returned on the root request", c.Path)
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}

	// The minted session must resolve to the authenticated identity, otherwise
	// "/" rejects the very next request.
	sess := s.lookupSession(c.Value)
	if sess == nil {
		t.Fatal("session cookie does not resolve to a session")
	}
	if sess.Username != "octocat" {
		t.Errorf("session username = %q, want octocat", sess.Username)
	}
	if sess.Role != config.RoleOwner {
		t.Errorf("session role = %q, want %q", sess.Role, config.RoleOwner)
	}

	// The C3 terminal-assertion mint lives in the same (formerly gated) path and
	// must survive the un-nesting.
	var terminal *http.Cookie
	for _, ck := range (&http.Response{Header: rec.Header()}).Cookies() {
		if ck.Name == terminalAssertionCookieName {
			terminal = ck
		}
	}
	if terminal == nil {
		t.Fatal("no terminal assertion cookie set — the C3 mint was lost from the device-flow path")
	}
	if terminal.Value == "" {
		t.Fatal("terminal assertion cookie set to an empty value")
	}
}

func TestCovDF_GHUserAuthStatus_Session(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	sid := s.createUserSession("alice", config.RoleOwner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	s.mux.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["logged_in"] != true || body["username"] != "alice" {
		t.Fatalf("session status wrong: %v", body)
	}
}

func TestCovDF_GHUserAuthStatus_HubHeader(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/gh-user-auth/status", nil)
	req.Header.Set("X-Hive-User", "hubby")
	req.Header.Set("X-Hive-Role", "read")
	s.mux.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["username"] != "hubby" {
		t.Fatalf("hub header status wrong: %v", body)
	}
}

func TestCovDF_GHUserAuthStatus_DirectRouteNoSession(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.Dashboard.AuthorizedUsers = []string{"someone"}
	deps.Config.Dashboard.HubProxied = false
	rec := doGet(s, "/api/gh-user-auth/status")
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["logged_in"] != false {
		t.Fatalf("direct-route no-session should be logged_in=false, got %v", body)
	}
}

func TestCovDF_GHUserAuthStatus_NoTokenFile(t *testing.T) {
	// Not direct-route, no session, no hub header → reads userTokenPath which is
	// absent in tests → logged_in false.
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doGet(s, "/api/gh-user-auth/status")
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["logged_in"] != false {
		t.Fatalf("no token file should be logged_in=false, got %v", body)
	}
}

func TestCovDF_GHUserAuthLogout_Session(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	sid := s.createUserSession("bob", config.RoleOwner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	s.mux.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "logged_out" {
		t.Fatalf("logout wrong: %v", body)
	}
}

func TestCovDF_GHUserAuthSession_Redirects(t *testing.T) {
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doGet(s, "/api/gh-user-auth/session")
	if rec.Code != http.StatusFound {
		t.Fatalf("session redirect: want 302, got %d", rec.Code)
	}
}

// ---- handleSSO ----

func TestCovDF_SSO_NoSecret(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "")
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doGet(s, "/sso?token=x")
	// Terminates with an explanation instead of redirecting to "/", which
	// bounced the browser in a loop.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret: want 503, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("no secret must not redirect, got Location %q", loc)
	}
}

func TestCovDF_SSO_MissingToken(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "shhh")
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doGet(s, "/sso")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing token: want 400, got %d", rec.Code)
	}
}

func TestCovDF_SSO_BadToken(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "shhh")
	s, _, _ := dfServer(t, "complete", "octocat")
	rec := doGet(s, "/sso?token=not-a-valid-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: want 401, got %d", rec.Code)
	}
}

func TestCovDF_SSO_ValidNoAllowlist(t *testing.T) {
	secret := "shared-secret-1234"
	t.Setenv("HIVE_HUB_SECRET", secret)
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.HiveID = "hive-abc"
	tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(secret), "alice", config.RoleOwner, "hive-abc", time.Now())
	rec := doGet(s, "/sso?token="+tok)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid sso: want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatalf("expected a session cookie to be set")
	}
}

func TestCovDF_SSO_ValidWithAllowlist(t *testing.T) {
	secret := "shared-secret-1234"
	t.Setenv("HIVE_HUB_SECRET", secret)
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.HiveID = "hive-abc"
	deps.Config.Dashboard.AuthorizedUsers = []string{"owner1:owner", "alice:read"}
	deps.Config.Dashboard.HubProxied = false
	tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(secret), "alice", config.RoleOwner, "hive-abc", time.Now())
	rec := doGet(s, "/sso?token="+tok)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("allowlisted sso: want 303, got %d", rec.Code)
	}
}

func TestCovDF_SSO_NotAuthorized(t *testing.T) {
	secret := "shared-secret-1234"
	t.Setenv("HIVE_HUB_SECRET", secret)
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.HiveID = "hive-abc"
	deps.Config.Dashboard.AuthorizedUsers = []string{"someoneelse"}
	deps.Config.Dashboard.HubProxied = false
	tok := hub.MintSSOToken(hub.SSOSigningSeedFromMaster(secret), "alice", config.RoleOwner, "hive-abc", time.Now())
	rec := doGet(s, "/sso?token="+tok)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized sso: want 403, got %d", rec.Code)
	}
}

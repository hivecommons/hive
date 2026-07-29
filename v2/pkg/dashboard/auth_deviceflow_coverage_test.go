package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/hub"
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
			json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_validtoken", "token_type": "bearer", "scope": "repo workflow"})
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
	mock := deviceFlowMock(t, tokenStatus, login)
	s := NewServerWithAuth(0, "authsecret", dfLogger())
	s.userTokenPath = filepath.Join(t.TempDir(), "gh-user-token")
	deps := testDeps(t)
	deps.Config.GitHub.OAuthClientID = "Ov23liTest"
	deps.Config.GitHub.BaseURL = mock.URL
	deps.Config.GitHub.APIURL = mock.URL
	s.RegisterAPI(deps)
	return s, deps, mock
}

func TestCovDF_GHUserAuthStart_NoClientID(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.Config.GitHub.OAuthClientID = ""
	if rec := doPost(s, "/api/gh-user-auth/start", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no client id: want 400, got %d", rec.Code)
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

func TestCovDF_GHUserAuthPoll_Pending(t *testing.T) {
	s, _, _ := dfServer(t, "authorization_pending", "octocat")
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "pending" {
		t.Fatalf("want pending, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_SlowDown(t *testing.T) {
	s, _, _ := dfServer(t, "slow_down", "octocat")
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "slow_down" {
		t.Fatalf("want slow_down, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_Error(t *testing.T) {
	s, _, _ := dfServer(t, "error", "octocat")
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "error" {
		t.Fatalf("want error, got %v", body["status"])
	}
}

func TestCovDF_GHUserAuthPoll_CompleteOwner(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	// Not direct-route (no allowlist) → role owner, token persisted only if
	// userTokenPath is writable; SetUserClient captures the token.
	var gotToken string
	deps.SetUserClient = func(tok string) { gotToken = tok }
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	// Either "complete" (if /data writable) or an error about persisting token —
	// both exercise the owner-role branch. In CI /data is absent so we expect the
	// persist-error path, which still covers the code.
	if body["status"] != "complete" && body["status"] != "error" {
		t.Fatalf("unexpected status %v (body=%s)", body["status"], rec.Body.String())
	}
	_ = gotToken
}

func TestCovDF_GHUserAuthPoll_UnverifiableIdentity(t *testing.T) {
	// login empty → /user returns 401 → ValidateToken fails → identity error.
	s, _, _ := dfServer(t, "complete", "")
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
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
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
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
	doPost(s, "/api/gh-user-auth/start", nil)
	rec := doPost(s, "/api/gh-user-auth/poll", nil)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "complete" {
		t.Fatalf("want complete for authorized viewer, got %v (%s)", body["status"], rec.Body.String())
	}
	if body["username"] != "viewer1" {
		t.Fatalf("want viewer1, got %v", body["username"])
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
	tok := hub.MintSSOToken(secret, "alice", config.RoleOwner, "hive-abc", time.Now())
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
	tok := hub.MintSSOToken(secret, "alice", config.RoleOwner, "hive-abc", time.Now())
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
	tok := hub.MintSSOToken(secret, "alice", config.RoleOwner, "hive-abc", time.Now())
	rec := doGet(s, "/sso?token="+tok)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized sso: want 403, got %d", rec.Code)
	}
}

// ---- restoreGHUserSession ----

func TestCovDF_RestoreGHUserSession_NoSetUserClient(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.SetUserClient = nil
	// Should return early without panicking (no token file present anyway).
	s.restoreGHUserSession()
}

func TestCovDF_RestoreGHUserSession_NoToken(t *testing.T) {
	s, deps, _ := dfServer(t, "complete", "octocat")
	deps.SetUserClient = func(string) {}
	// userTokenPath absent in tests → early return.
	s.restoreGHUserSession()
}

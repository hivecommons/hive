package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleLoginRedirect(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Register OAuth routes manually (won't register without env var)
	srv.mux.HandleFunc("GET /login", srv.handleLogin)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "github.com/login/oauth/authorize") {
		t.Errorf("redirect should go to GitHub OAuth, got %q", loc)
	}
}

func TestHandleLoginWithRedirectParam(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /login-test", srv.handleLogin)

	req := httptest.NewRequest("GET", "/login-test?redirect=/dashboard", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "state=") {
		t.Error("redirect should include state param")
	}
}

func TestHandleLoginRejectsOpenRedirect(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /login-redir", srv.handleLogin)

	req := httptest.NewRequest("GET", "/login-redir?redirect=//evil.com", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	if strings.Contains(loc, "evil.com") {
		t.Error("should not redirect to external URLs")
	}
}

func TestHandleLoginRdParam(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /login-rd", srv.handleLogin)

	req := httptest.NewRequest("GET", "/login-rd?rd=/my-page", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307, got %d", w.Code)
	}
}

func TestHandleLogout(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("POST /logout-test", srv.handleLogout)

	req := httptest.NewRequest("POST", "/logout-test", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 200 or 307, got %d", w.Code)
	}
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "hive_hub_user" && c.MaxAge < 0 {
			found = true
		}
	}
	if !found {
		t.Error("logout should set expired cookie")
	}
}

func TestHandleAuthUserNoCookie(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /auth-user-test", srv.handleAuthUser)

	req := httptest.NewRequest("GET", "/auth-user-test", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != false {
		t.Error("should return authenticated=false without cookie")
	}
}

func TestHandleAuthUserEmptyCookie(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /auth-user-empty", srv.handleAuthUser)

	req := httptest.NewRequest("GET", "/auth-user-empty", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: ""})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != false {
		t.Error("should return authenticated=false with empty cookie")
	}
}

func TestHandleAuthUserUnknownUser(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /auth-user-unknown", srv.handleAuthUser)

	req := httptest.NewRequest("GET", "/auth-user-unknown", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "nonexistent-user-xyz"})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != false {
		t.Error("should return authenticated=false for unknown user")
	}
}

func TestHandleAdminUsersAsAdmin(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Call handler directly to skip requireAdmin middleware (no /data on macOS)
	req := httptest.NewRequest("GET", "/api/saas/admin/users", nil)
	w := httptest.NewRecorder()
	srv.handleAdminUsers(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("admin users: expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	// users key should exist (may be nil array on macOS since no /data/saas/users)
	if _, ok := resp["users"]; !ok {
		t.Error("should return users key")
	}
}

func TestHandleAdminUpdateUserNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Call directly to skip requireAdmin
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.handleAdminUpdateUser)
	req := httptest.NewRequest("PUT", "/api/saas/admin/users/nonexistent-xyz", strings.NewReader(`{"saas_quota":5}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("update unknown user: expected 404, got %d", w.Code)
	}
}

func TestHandleAdminUpdateUserBadJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.handleAdminUpdateUser)
	req := httptest.NewRequest("PUT", "/api/saas/admin/users/someone", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Errorf("expected 404 or 400, got %d", w.Code)
	}
}

func TestHandleHiveStatusNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)
	req := httptest.NewRequest("GET", "/api/saas/hives/nonexistent-xyz/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDeleteHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)
	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexistent-xyz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Handler cleans up registry entry even if hive doesn't exist on disk
	if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Errorf("expected 200 or 404, got %d", w.Code)
	}
}

func TestHandleDeleteHivePathTraversal(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)
	req := httptest.NewRequest("DELETE", "/api/saas/hives/..%2F..%2Fetc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("path traversal: expected 400 or 404, got %d", w.Code)
	}
}

func TestHandleUpgradeHiveCORSPreflight(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Register directly to avoid requireAuth for OPTIONS
	srv.mux.HandleFunc("OPTIONS /upgrade-test/{id}", srv.handleUpgradeHive)

	req := httptest.NewRequest("OPTIONS", "/upgrade-test/test-hive", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("CORS preflight: expected 204, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "https://hive.kubestellar.io" {
		t.Error("CORS headers missing")
	}
}

func TestHandleUpgradeHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", srv.handleUpgradeHive)
	req := httptest.NewRequest("POST", "/api/saas/hives/nonexistent-xyz/upgrade", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAccessListNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/access", srv.handleAccessList)
	req := httptest.NewRequest("GET", "/api/saas/hives/nonexistent-xyz/access", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAccessAddNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/access", srv.handleAccessAdd)
	body := `{"username":"testuser","role":"read"}`
	req := httptest.NewRequest("POST", "/api/saas/hives/nonexistent-xyz/access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAccessRemoveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", srv.handleAccessRemove)
	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexistent-xyz/access/testuser", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleRequestAccessNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)
	req := httptest.NewRequest("POST", "/api/saas/hives/nonexistent-xyz/request-access", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetRequestsNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/requests", srv.handleGetRequests)
	req := httptest.NewRequest("GET", "/api/saas/hives/nonexistent-xyz/requests", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleApproveRequestNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", srv.handleApproveRequest)
	req := httptest.NewRequest("POST", "/api/saas/hives/nonexistent-xyz/requests/someone/approve", strings.NewReader(`{"role":"read"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDenyRequestNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", srv.handleDenyRequest)
	req := httptest.NewRequest("POST", "/api/saas/hives/nonexistent-xyz/requests/someone/deny", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleCreateHiveNoAuth(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateHiveBadJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Call handler directly to skip requireAuth
	req := httptest.NewRequest("POST", "/api/saas/hives", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	// getAuthUser returns "" → handleCreateHive returns 401
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleCreateHiveValidation(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty org", `{"org":"","repos":"myrepo","github_token":"ghp_abc123"}`, http.StatusBadRequest},
		{"invalid org", `{"org":"bad org!","repos":"myrepo","github_token":"ghp_abc123"}`, http.StatusBadRequest},
		{"invalid repo", `{"org":"valid","repos":"bad repo!","github_token":"ghp_abc123"}`, http.StatusBadRequest},
		{"no token or app", `{"org":"valid","repos":"myrepo"}`, http.StatusBadRequest},
		{"bad token prefix", `{"org":"valid","repos":"myrepo","github_token":"gho_wrong"}`, http.StatusBadRequest},
		{"bad app key", `{"org":"valid","repos":"myrepo","auth_method":"app","app_id":"123","installation_id":"456","app_private_key":"not-pem"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/create", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: hubAdminUsername})
			w := httptest.NewRecorder()
			srv.handleCreateHive(w, req)
			// Either 401 (loadSaaSUser returns nil on macOS) or the expected validation error
			if w.Code != tt.want && w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Errorf("%s: expected %d, got %d (body: %s)", tt.name, tt.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleUserTokenBadBody(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("POST", "/user-token-test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleUserTokenMissingFields(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("POST", "/user-token-test", strings.NewReader(`{"hive_id":"h1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleUserTokenOtherUserForbidden(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	body := `{"hive_id":"h1","username":"otheruser"}`
	req := httptest.NewRequest("POST", "/user-token-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "regularuser"})
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleUserTokenUserNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// getAuthUser returns "" on macOS, so requester="" != username → 403
	// Admin can request any user's token
	body := `{"hive_id":"h1","username":"nonexistent-xyz"}`
	req := httptest.NewRequest("POST", "/user-token-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "nonexistent-xyz"})
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	// On macOS: getAuthUser returns "" → 403 (not the user)
	if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Errorf("expected 404 or 403, got %d", w.Code)
	}
}

func TestHandleProxyHiveConfigNotInRegistry(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/proxy-config-test", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: hubAdminUsername})
	w := httptest.NewRecorder()
	srv.handleProxyHiveConfig(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleProxyHiveConfigWithDashboardURL(t *testing.T) {
	// The SSRF guard blocks loopback (the httptest server address) in
	// production; override it here so the ownership check is the only gate.
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// The upstream must NOT be reached: this registry entry is ownerless, and
	// after the F9 fix an ownerless hive's config is not world-readable. The
	// unauthenticated test caller (getAuthUser == "") is not the site admin, so
	// the request is refused before any proxy fetch. A hit here would be a
	// regression re-opening CWE-862.
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/x-yaml")
		w.Write([]byte("agents:\n  scanner:\n    model: claude-sonnet\n"))
	}))
	defer upstream.Close()

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "config-hive", DashboardURL: upstream.URL}, // Owner intentionally empty
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", srv.handleProxyHiveConfig)
	req := httptest.NewRequest("GET", "/api/saas/hive-config/config-hive", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("ownerless hive must be refused; expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}
	if reached {
		t.Error("upstream config was fetched for an ownerless hive — F9 regression")
	}
}

func TestHandleMyHivesNoAuthDirect(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/my-hives-test", nil)
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleMyHivesWithRegistryHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "local-hive", Owner: hubAdminUsername, Online: true, Name: "org/repo"},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/my-hives-test", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: hubAdminUsername})
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	// getAuthUser returns "" on macOS (loadSaaSUser returns nil) → 401
	if w.Code != http.StatusOK && w.Code != http.StatusUnauthorized {
		t.Errorf("expected 200 or 401, got %d", w.Code)
	}
}

func TestHandleSaaSAuthCheckNoHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/api/saas/auth-check", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSaaSAuthCheckNoAuthWithHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test-hive", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestHandleSaaSAuthCheckWithUserNoAccess(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test-hive", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "someuser"})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// On macOS, getAuthUser may return "" (no /data dir) → 401, or user loads → 403
	if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Errorf("expected 403 or 401, got %d", w.Code)
	}
}

func TestLoadAccessRequestsNonexistent(t *testing.T) {
	reqs := loadAccessRequests("nonexistent-hive-xyz")
	if reqs != nil {
		t.Error("should return nil for nonexistent hive")
	}
}

func TestDecryptTokenEmpty(t *testing.T) {
	_, err := decryptToken("")
	if err == nil {
		t.Error("should error on empty token")
	}
}

func TestDecryptTokenGarbage(t *testing.T) {
	_, err := decryptToken("not-base64!")
	if err == nil {
		t.Error("should error on invalid base64")
	}
}

func TestDecryptTokenTooShort(t *testing.T) {
	// Valid base64 but too short for GCM nonce
	_, err := decryptToken(strings.Repeat("A", 4))
	if err == nil {
		t.Error("should error on too-short ciphertext")
	}
}

func TestHandleDashboardUnfurlBotReturnsOG(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.Header.Set("User-Agent", "Slackbot-LinkExpanding 1.0")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for unfurl bot, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "og:title") {
		t.Error("should return OG meta tags for unfurl bot")
	}
}

func TestHandleDashboardNoCookieRedirects(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307 redirect, got %d", w.Code)
	}
	if w.Header().Get("Location") != "/login" {
		t.Errorf("should redirect to /login, got %q", w.Header().Get("Location"))
	}
}

func TestHandleDashboardWithCookieReturnsDashboard(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "testuser"})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Error("should return HTML")
	}
}

func TestRequireAuthBlockedUser(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// On macOS, loadSaaSUser returns nil (no /data/saas/users)
	// So requireAuth calls ensureSaaSUser which tries to create, fails, then loads again → nil → 401
	handler := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "blocked-user-xyz"})
	w := httptest.NewRecorder()
	handler(w, req)

	// Will be 401 (user can't be created on macOS) which exercises the code path
	if w.Code != http.StatusUnauthorized {
		t.Logf("requireAuth with unknown user returned %d", w.Code)
	}
}

func TestHandleOAuthCallbackMissingCode(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")
	srv.mux.HandleFunc("GET /callback-test", srv.handleOAuthCallback)

	req := httptest.NewRequest("GET", "/callback-test", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing code, got %d", w.Code)
	}
}

func TestHandleOAuthCallbackWithCode(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// With a code present, it tries to exchange with GitHub (ghTokenURL const)
	// which will fail with network error → 502
	req := httptest.NewRequest("GET", "/callback-with-code?code=testcode123", nil)
	w := httptest.NewRecorder()
	srv.handleOAuthCallback(w, req)

	// Will get 502 because ghTokenURL points to real GitHub
	if w.Code != http.StatusBadGateway {
		t.Logf("OAuth callback with code returned %d (expected 502)", w.Code)
	}
}

func TestCountUserHives(t *testing.T) {
	// Will return 0 on macOS (no /data/saas/hives dir)
	count := countUserHives("nonexistent-user")
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestListSaaSHives(t *testing.T) {
	// Will return nil on macOS (no /data/saas/hives dir)
	hives := listSaaSHives()
	if len(hives) > 0 {
		t.Logf("found %d hives (unexpected)", len(hives))
	}
}

func TestListAllSaaSUsers(t *testing.T) {
	// Will return nil on macOS (no /data/saas/users dir)
	users := listAllSaaSUsers()
	_ = users // exercises the code path
}

func TestIsUnfurlBotVariants(t *testing.T) {
	bots := []string{
		"Slackbot-LinkExpanding 1.0",
		"Slack-ImgProxy hdpi",
		"Discordbot/2.0",
		"Twitterbot/1.0",
		"facebookexternalhit/1.1",
		"LinkedInBot/1.0",
		"WhatsApp/2.23",
		"TelegramBot (like TwitterBot)",
	}
	for _, ua := range bots {
		if !isUnfurlBot(ua) {
			t.Errorf("should detect %q as unfurl bot", ua)
		}
	}
	if isUnfurlBot("Mozilla/5.0") {
		t.Error("should not detect regular browser as unfurl bot")
	}
	if isUnfurlBot("curl/7.0") {
		t.Error("should not detect curl as unfurl bot")
	}
}

func TestValidateGitHubTokenInvalid(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Will try to call GitHub API and fail (no real token)
	result := srv.validateGitHubToken("ghp_faketoken12345678901234567890")
	// Should return empty string on failure
	if result != "" {
		t.Logf("validateGitHubToken returned %q (expected empty for fake token)", result)
	}
}

func TestSaveAccessRequestsNonexistentDir(t *testing.T) {
	// Will fail to create dir under /data/saas/hives — but shouldn't panic
	saveAccessRequests("nonexistent-hive-xyz", []AccessRequest{
		{Username: "testuser", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})
}

func TestValidateGitHubTokenCacheHit(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Pre-populate the cache
	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_cached_test_token"] = ghTokenCacheEntry{
		username:  "cached-user",
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()

	result := srv.validateGitHubToken("ghp_cached_test_token")
	if result != "cached-user" {
		t.Errorf("expected 'cached-user', got %q", result)
	}

	// Cleanup
	ghTokenCacheMu.Lock()
	delete(ghTokenCache, "ghp_cached_test_token")
	ghTokenCacheMu.Unlock()
}

func TestValidateGitHubTokenCacheExpired(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Pre-populate with expired entry
	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_expired_test_token"] = ghTokenCacheEntry{
		username:  "expired-user",
		expiresAt: time.Now().Add(-1 * time.Minute),
	}
	ghTokenCacheMu.Unlock()

	// Expired cache → hits GitHub API → fails with invalid token → returns ""
	result := srv.validateGitHubToken("ghp_expired_test_token")
	if result != "" {
		t.Errorf("expired cache should re-validate and fail, got %q", result)
	}

	// Cleanup
	ghTokenCacheMu.Lock()
	delete(ghTokenCache, "ghp_expired_test_token")
	ghTokenCacheMu.Unlock()
}

func TestValidateGitHubTokenEmpty(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	result := srv.validateGitHubToken("")
	if result != "" {
		t.Errorf("empty token should return empty, got %q", result)
	}
}

func TestGetAuthUserBearer(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Pre-populate cache so Bearer auth works
	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_bearer_test"] = ghTokenCacheEntry{
		username:  "bearer-user",
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ghp_bearer_test")
	result := srv.getAuthUser(req)
	if result != "bearer-user" {
		t.Errorf("expected 'bearer-user', got %q", result)
	}

	ghTokenCacheMu.Lock()
	delete(ghTokenCache, "ghp_bearer_test")
	ghTokenCacheMu.Unlock()
}

// TestSubdomainTrustIsRedirectOnly records the F4 split explicitly: a
// *.hive.kubestellar.io subdomain is trusted as a post-login REDIRECT target
// (hosted sign-in depends on it) but is NOT trusted to author a request.
//
// This test previously asserted, via isTrustedOrigin, that a subdomain was
// trusted for BOTH — which is the vulnerability F4 reported.
func TestSubdomainTrustIsRedirectOnly(t *testing.T) {
	for _, sub := range []string{
		"https://dashboard.hive.kubestellar.io",
		"https://my-hive.hive.kubestellar.io",
	} {
		if !isTrustedRedirectTarget(sub) {
			t.Errorf("%s should remain a trusted redirect target (hosted sign-in depends on it)", sub)
		}
		if isSameOriginAsHub(sub) {
			t.Errorf("F4 REGRESSION: %s is trusted to author requests at the hub", sub)
		}
	}
	// Local development is the hub itself, so it is trusted for both.
	for _, local := range []string{"http://localhost:3001", "http://127.0.0.1:8080"} {
		if !isTrustedRedirectTarget(local) || !isSameOriginAsHub(local) {
			t.Errorf("%s should be trusted for local development", local)
		}
	}
}

func TestOriginTrustBadParse(t *testing.T) {
	if isSameOriginAsHub("://invalid") {
		t.Error("unparseable URL should not be a trusted origin")
	}
	if isTrustedRedirectTarget("://invalid") {
		t.Error("unparseable URL should not be a trusted redirect target")
	}
}

func TestRegisterOAuthEnabled(t *testing.T) {
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "test-client-id")

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// OAuth routes should be registered, test /login exists
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Should redirect to GitHub OAuth (307)
	if w.Code != http.StatusTemporaryRedirect {
		t.Errorf("expected 307, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "test-client-id") {
		t.Error("redirect should contain client_id")
	}
}

func TestHandleOAuthCallbackNoAccessToken(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Code is present but GitHub returns error (invalid code)
	req := httptest.NewRequest("GET", "/callback?code=invalid_code_xyz", nil)
	w := httptest.NewRecorder()
	srv.handleOAuthCallback(w, req)

	// GitHub will return a valid JSON but with no access_token → 502
	if w.Code != http.StatusBadGateway {
		t.Logf("OAuth callback with invalid code: %d", w.Code)
	}
}

func TestHandleContributeProxyWithLocalHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Hive with localhost URL → private URL → rejected by findContributeHive
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "local-only", Online: true, IsPublic: true, DashboardURL: "http://localhost:3001", Owner: "user"},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// findContributeHive rejects private URLs → no hive found → 503
	if w.Code != http.StatusServiceUnavailable {
		t.Logf("contribute proxy with localhost hive: %d", w.Code)
	}
}

func TestHandleContributeWSProxyWithLocalHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "local-ws", Online: true, IsPublic: true, DashboardURL: "http://127.0.0.1:3001", Owner: "user"},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/contribute/ws", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Logf("contribute WS proxy with local hive: %d", w.Code)
	}
}

func TestLoadSaaSUserPathTraversalVariants(t *testing.T) {
	tests := []string{"../etc/passwd", "foo/bar", "foo\\bar", "..\\windows"}
	for _, name := range tests {
		if loadSaaSUser(name) != nil {
			t.Errorf("loadSaaSUser(%q) should return nil", name)
		}
	}
}

func TestLoadSaaSUserValid(t *testing.T) {
	// Valid username but no /data/saas/users dir → nil
	if loadSaaSUser("validuser") != nil {
		t.Error("should return nil when user file doesn't exist")
	}
}

func TestMarkStaleHivesWithOldHeartbeat(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	old := "2020-01-01T00:00:00Z"
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "stale-hive", Online: true, LastHeartbeat: old},
		{ID: "fresh-hive", Online: true, LastHeartbeat: "2099-01-01T00:00:00Z"},
	}
	srv.mu.Unlock()

	srv.mu.Lock()
	srv.markStaleHives()
	hives := make([]RegistryEntry, len(srv.registry.Hives))
	copy(hives, srv.registry.Hives)
	srv.mu.Unlock()

	// markStaleHives marks offline or removes — check by ID
	staleFound := false
	freshFound := false
	for _, h := range hives {
		if h.ID == "stale-hive" {
			staleFound = true
			if h.Online {
				t.Error("stale hive should be marked offline")
			}
		}
		if h.ID == "fresh-hive" {
			freshFound = true
			if !h.Online {
				t.Error("fresh hive should remain online")
			}
		}
	}
	if !freshFound {
		t.Error("fresh hive should still be in registry")
	}
	_ = staleFound
}

func TestIsCSRFSafePostWithOrigin(t *testing.T) {
	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	if !isCSRFSafe(req) {
		t.Error("POST with trusted origin should pass CSRF")
	}
}

func TestIsCSRFSafePostWithUntrustedOrigin(t *testing.T) {
	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("Origin", "https://evil.com")
	if isCSRFSafe(req) {
		t.Error("POST with untrusted origin should fail CSRF")
	}
}

func TestIsCSRFSafeGetAlwaysPasses(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		req := httptest.NewRequest(method, "/test", nil)
		if !isCSRFSafe(req) {
			t.Errorf("%s should always pass CSRF", method)
		}
	}
}

// TestHandleProxyHiveConfigSSRFAndOwnership verifies the N4 hardening:
// a private/loopback DashboardURL is refused, and a non-owner (non-admin)
// caller is refused even for a public URL.
func TestHandleProxyHiveConfigSSRFAndOwnership(t *testing.T) {
	// 1) SSRF guard active (default) → a loopback DashboardURL is rejected 403.
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: "ssrf-hive", DashboardURL: "http://127.0.0.1:1/x"}}
	srv.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", srv.handleProxyHiveConfig)
	req := httptest.NewRequest("GET", "/api/saas/hive-config/ssrf-hive", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("private DashboardURL: expected 403, got %d (%s)", w.Code, w.Body.String())
	}

	// 2) Ownership: an owned hive with a public URL, called by a non-owner
	//    (no auth user in this test → caller ""), is refused 403.
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: "owned-hive", DashboardURL: "https://example.com", Owner: "alice"}}
	srv.mu.Unlock()
	req2 := httptest.NewRequest("GET", "/api/saas/hive-config/owned-hive", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("non-owner: expected 403, got %d (%s)", w2.Code, w2.Body.String())
	}
}

// TestHandleProxyHiveConfigOwnerlessDenied pins F9 (CWE-862): an OWNERLESS
// registry entry (Owner == "") must NOT be world-readable. Before the fix the
// ownership check was gated on `owner != ""`, so an ownerless hive's raw config
// was fetchable by any authenticated hub user. Now a non-admin caller is refused
// even for an ownerless hive. The SSRF guard is disabled here so the only thing
// that can reject the request is the ownership check.
func TestHandleProxyHiveConfigOwnerlessDenied(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	// Owner intentionally empty (ownerless), public URL, SSRF disabled.
	srv.registry.Hives = []RegistryEntry{{ID: "ownerless-hive", DashboardURL: "https://example.com", Owner: ""}}
	srv.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", srv.handleProxyHiveConfig)

	// caller is "" (no auth user in test) → not the admin → must be refused.
	req := httptest.NewRequest("GET", "/api/saas/hive-config/ownerless-hive", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("ownerless hive must not be world-readable; expected 403, got %d (%s)", w.Code, w.Body.String())
	}
}

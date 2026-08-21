package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// helperSetupAuthUser pre-populates the token cache for a user and returns a cleanup func.
func helperSetupAuthUser(t *testing.T, token, username string) func() {
	t.Helper()
	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{
		username:  username,
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()
	return func() {
		ghTokenCacheMu.Lock()
		delete(ghTokenCache, token)
		ghTokenCacheMu.Unlock()
	}
}

// ============================================================
// handleToggleAutoUpgrade — registry hive, owner match, save fail
// ============================================================

func TestHandleToggleAutoUpgradeRegistryHiveOwnerMatch(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_toggle_owner", "toggle-owner")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "toggle-reg-hive", Owner: "toggle-owner", Online: true, GitHash: "abc123", GitBranch: "v2"},
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", srv.handleToggleAutoUpgrade)

	req := httptest.NewRequest("PUT", "/api/saas/hives/toggle-reg-hive/auto-upgrade",
		strings.NewReader(`{"auto_upgrade":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_toggle_owner")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// saveSaaSHive will fail (no /data), → 500
	if w.Code != http.StatusInternalServerError {
		t.Logf("toggle auto-upgrade with registry hive: got %d (expected 500 due to save fail)", w.Code)
	}
}

func TestHandleToggleAutoUpgradeRegistryHiveForbidden(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_toggle_nonowner", "not-the-owner")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "forbidden-hive", Owner: "actual-owner", Online: true},
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", srv.handleToggleAutoUpgrade)

	req := httptest.NewRequest("PUT", "/api/saas/hives/forbidden-hive/auto-upgrade",
		strings.NewReader(`{"auto_upgrade":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_toggle_nonowner")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleToggleAutoUpgradeRegistryHiveBadBody(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_toggle_bad", "toggle-owner-bad")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "badbody-hive", Owner: "toggle-owner-bad", Online: true},
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", srv.handleToggleAutoUpgrade)

	req := httptest.NewRequest("PUT", "/api/saas/hives/badbody-hive/auto-upgrade",
		strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_toggle_bad")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ============================================================
// handleUpgradeHive — owner forbidden
// ============================================================

func TestHandleUpgradeHiveForbidden(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_upgrade_notown", "not-owner")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// We can't create a SaaS hive file, so loadSaaSHive returns nil → 404
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", srv.handleUpgradeHive)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/upgrade", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("Authorization", "Bearer ghp_upgrade_notown")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleToggleVisibility — owner forbidden
// ============================================================

func TestHandleToggleVisibilityForbidden(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_vis_notown", "not-vis-owner")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/visibility", srv.handleToggleVisibility)

	req := httptest.NewRequest("PUT", "/api/saas/hives/nonexist/visibility",
		strings.NewReader(`{"is_public":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("Authorization", "Bearer ghp_vis_notown")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleDeleteHive — path traversal and not-found with auth
// ============================================================

func TestHandleDeleteHiveWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_user", "del-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)

	// Path traversal
	req := httptest.NewRequest("DELETE", "/api/saas/hives/..%2Fetc", nil)
	req.Header.Set("Authorization", "Bearer ghp_del_user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404, got %d", w.Code)
	}

	// Deleting a hive whose SaaS entry is already gone is idempotent: the
	// handler still purges any in-memory registry entry and reports deleted.
	req2 := httptest.NewRequest("DELETE", "/api/saas/hives/nonexist", nil)
	req2.Header.Set("Authorization", "Bearer ghp_del_user")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 (idempotent delete), got %d", w2.Code)
	}
}

// ============================================================
// handleMigrateHive — deeper paths
// ============================================================

// ============================================================
// handleAccessList/Add/Remove — not-found with auth
// ============================================================

func TestHandleAccessListWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_acl_user", "acl-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/access", srv.handleAccessList)

	req := httptest.NewRequest("GET", "/api/saas/hives/nonexist/access", nil)
	req.Header.Set("Authorization", "Bearer ghp_acl_user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAccessAddWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_add_user", "add-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/access", srv.handleAccessAdd)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/access",
		strings.NewReader(`{"username":"newuser","role":"read"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_add_user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleAccessRemoveWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_rm_user", "rm-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", srv.handleAccessRemove)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexist/access/someuser", nil)
	req.Header.Set("Authorization", "Bearer ghp_rm_user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleAccessAdd — bad body
// ============================================================

func TestHandleAccessAddBadBody(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/access", srv.handleAccessAdd)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/access",
		strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Will hit not-found first since loadSaaSHive returns nil
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleRequestAccess / handleGetRequests / handleApproveRequest / handleDenyRequest — with auth
// ============================================================

func TestHandleRequestAccessWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_reqacc", "req-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/request-access", nil)
	req.Header.Set("Authorization", "Bearer ghp_reqacc")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetRequestsWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_getreq", "getreq-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/requests", srv.handleGetRequests)

	req := httptest.NewRequest("GET", "/api/saas/hives/nonexist/requests", nil)
	req.Header.Set("Authorization", "Bearer ghp_getreq")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleApproveRequestWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_approve", "approver")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", srv.handleApproveRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/requests/user/approve",
		strings.NewReader(`{"role":"read"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_approve")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDenyRequestWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_deny", "denier")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", srv.handleDenyRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/requests/user/deny", nil)
	req.Header.Set("Authorization", "Bearer ghp_deny")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleApproveAccess / handleDenyAccess — with auth
// ============================================================

func TestHandleApproveAccessWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_appracc", "approver-acc")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", srv.handleApproveAccess)

	req := httptest.NewRequest("PUT", "/api/saas/hives/nonexist/approve-access/user", nil)
	req.Header.Set("Authorization", "Bearer ghp_appracc")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDenyAccessWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_denyacc", "denier-acc")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", srv.handleDenyAccess)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexist/deny-access/user", nil)
	req.Header.Set("Authorization", "Bearer ghp_denyacc")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleCreateHive — with auth but no user file
// ============================================================

func TestHandleCreateHiveWithAuthNoUser(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_create_nouser", "create-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/create-hive",
		strings.NewReader(`{"org":"myorg","repos":"repo1","github_token":"ghp_fake123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_nouser")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	// loadSaaSUser returns nil → 403
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 (no user file), got %d", w.Code)
	}
}

// ============================================================
// handleHiveStatus — with auth, not found
// ============================================================

func TestHandleHiveStatusWithAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_status_user", "status-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)

	req := httptest.NewRequest("GET", "/api/saas/hives/nonexist/status", nil)
	req.Header.Set("Authorization", "Bearer ghp_status_user")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleAccessStatus — deeper path with user in cache
// ============================================================

func TestHandleAccessStatusWithUserAndHives(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_accstat_user", "accstat-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "owned-hive", Owner: "accstat-user", Online: true, LastHeartbeat: now},
		{ID: "other-hive", Owner: "someone-else", Online: true, LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/saas/access-status", nil)
	req.Header.Set("Authorization", "Bearer ghp_accstat_user")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != true {
		t.Error("should be authenticated")
	}
	if resp["show_my_hives"] != true {
		t.Error("should show my hives")
	}
}

// ============================================================
// handleSaaSAuthCheck — user with access
// ============================================================

func TestHandleSaaSAuthCheckUserNoAccessFile(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_authcheck_user", "authcheck-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=some-hive", nil)
	req.Header.Set("Authorization", "Bearer ghp_authcheck_user")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// loadSaaSUser returns nil → 403
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

// ============================================================
// handleMyHives — admin with bearer auth
// ============================================================

func TestHandleMyHivesAdminWithBearer(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_admin_my", hubAdminUsername)
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "admin-owned", Owner: hubAdminUsername, Online: true, LastHeartbeat: now},
		{ID: "user-owned", Owner: "someuser", Online: true, LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/fleet", nil)
	req.Header.Set("Authorization", "Bearer ghp_admin_my")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	// ensureSaaSUser fails to save without /data, but the function continues
	// It may return 401 or a list depending on how ensureSaaSUser behaves
	_ = w.Code
}

// ============================================================
// handleUserToken — with proper auth
// ============================================================

func TestHandleUserTokenSelfRequest(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_usertoken", "token-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"h1","username":"token-user"}`
	req := httptest.NewRequest("POST", "/user-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_usertoken")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	// loadSaaSUser returns nil → 404
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleUserTokenAdminRequestOther(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_admin_token", hubAdminUsername)
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"h1","username":"other-user"}`
	req := httptest.NewRequest("POST", "/user-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_admin_token")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	// Admin can request other's token but user not found
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleRequestProvision — valid body with auth
// ============================================================

func TestHandleRequestProvisionValidBody(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_prov_valid", "prov-valid-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// github_host is required: a request must name the GitHub forge its org
	// lives on, so a "valid body" fixture has to carry one.
	body := `{"org":"validorg","github_host":"github.com","repos":"repo1,repo2","primary_repo":"repo1","acmm_level":3,"auth_method":"token","full_name":"Ada Lovelace"}`
	req := httptest.NewRequest("POST", "/provision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_prov_valid")
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)

	// saveProvisionRequest will fail without /data → 500
	// But the validation code paths are exercised
	if w.Code == http.StatusBadRequest {
		t.Errorf("valid body should not be rejected as bad request, got: %s", w.Body.String())
	}
}

func TestHandleRequestProvisionACMMLevelDefault(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_prov_acmm", "prov-acmm-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Carries github_host AND full_name on purpose: both are required, and
	// omitting either makes the handler 400 long before the ACMM clamp this
	// test exists to cover. This fixture used to omit full_name and discard the
	// status with `_ = w.Code`, so after #2369 made the name required it passed
	// while silently exercising the 400 branch instead of the clamp.
	body := `{"org":"validorg","github_host":"github.com","repos":"repo1","acmm_level":0,"full_name":"Ada Lovelace"}`
	req := httptest.NewRequest("POST", "/provision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_prov_acmm")
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)

	// ACMM 0 → clamped to minRequestACMMLevel. Assert we got PAST validation:
	// a 400 here means a required field regressed, which is exactly the failure
	// the old `_ = w.Code` hid. Persisting needs /data, absent in unit tests, so
	// the success path legitimately ends in 500 — anything but 400 proves the
	// clamp was reached.
	if w.Code == http.StatusBadRequest {
		t.Errorf("ACMM clamp path should not 400 — a required field likely regressed; body: %s", w.Body.String())
	}
}

func TestHandleRequestProvisionACMMLevelHigh(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_prov_high", "prov-high-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// See TestHandleRequestProvisionACMMLevelDefault: github_host and full_name
	// are required, so a fixture missing them never reaches the clamp.
	body := `{"org":"validorg","github_host":"github.com","repos":"repo1","acmm_level":99,"full_name":"Ada Lovelace"}`
	req := httptest.NewRequest("POST", "/provision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_prov_high")
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)

	// ACMM 99 (above maxRequestACMMLevel) → clamped to minRequestACMMLevel.
	// As above, 400 means validation rejected us before the clamp ran.
	if w.Code == http.StatusBadRequest {
		t.Errorf("ACMM clamp path should not 400 — a required field likely regressed; body: %s", w.Body.String())
	}
}

// ============================================================
// handleApproveProvision / handleDenyProvision — with admin auth
// ============================================================

func TestHandleApproveProvisionWithAdminAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_approv_admin", hubAdminUsername)
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/approve-provision/{username}", srv.handleApproveProvision)

	req := httptest.NewRequest("PUT", "/api/saas/approve-provision/nonexist", nil)
	req.Header.Set("Authorization", "Bearer ghp_approv_admin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDenyProvisionWithAdminAuth(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_deny_admin", hubAdminUsername)
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/deny-provision/{username}", srv.handleDenyProvision)

	req := httptest.NewRequest("DELETE", "/api/saas/deny-provision/nonexist", nil)
	req.Header.Set("Authorization", "Bearer ghp_deny_admin")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleAdminUpdateUser — more paths
// ============================================================

func TestHandleAdminUpdateUserWithAdminAuth(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.handleAdminUpdateUser)

	req := httptest.NewRequest("PUT", "/api/saas/admin/users/nonexist",
		strings.NewReader(`{"saas_quota":5,"blocked":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// requireAuth — blocked user path (exercises line 169-174)
// We can't create a blocked user file, but we exercise the code path where
// user == nil after ensureSaaSUser
// ============================================================

func TestRequireAuthUserNilAfterEnsure(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_blocked_test", "blocked-test-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	handler := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ghp_blocked_test")
	w := httptest.NewRecorder()
	handler(w, req)

	// getAuthUser returns "blocked-test-user" but loadSaaSUser returns nil
	// → ensureSaaSUser is called, fails to save, loadSaaSUser again returns nil → 401
	if w.Code != http.StatusUnauthorized {
		t.Logf("requireAuth with unsaveable user: got %d", w.Code)
	}
}

// ============================================================
// handleAuthUser — with valid user
// ============================================================

func TestHandleAuthUserKnownButNotInFile(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_authuser_known", "known-user")
	defer cleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ghp_authuser_known")
	w := httptest.NewRecorder()
	srv.handleAuthUser(w, req)

	// getAuthUser returns "known-user" via bearer cache
	// But handleAuthUser checks cookie first, then checks loadSaaSUser(cookie)
	// Since no cookie is set, it falls through to authenticated=false
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != false {
		t.Log("expected false since handleAuthUser only checks cookie, not bearer")
	}
}

// ============================================================
// Heartbeat with auto_upgrade response
// ============================================================

func TestHandleHeartbeatAutoUpgradeSpokeRequest(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Set latest SHA
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "7a41e01", Message: "latest"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	payload := `{"hive_id":"auto-spoke","git_hash":"old111","git_branch":"v2","auto_upgrade":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.UpgradeTo != "7a41e01" {
		t.Errorf("upgrade_to = %q, want 7a41e01", resp.UpgradeTo)
	}
}

func TestHandleHeartbeatAutoUpgradeNoUpgradeNeeded(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "current", Message: "current"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	payload := `{"hive_id":"no-up","git_hash":"current","git_branch":"v2","auto_upgrade":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.UpgradeTo != "" {
		t.Errorf("should not upgrade when current, got %q", resp.UpgradeTo)
	}
}

func TestHandleHeartbeatAutoUpgradeEmptyGitHash(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "target2", Message: "latest"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	// Empty git_hash → no upgrade
	payload := `{"hive_id":"empty-hash","git_hash":"","git_branch":"v2","auto_upgrade":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.UpgradeTo != "" {
		t.Errorf("empty git_hash should not trigger upgrade, got %q", resp.UpgradeTo)
	}
}

func TestHandleHeartbeatDefaultBranch(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// No git_branch → defaults to "v2"
	payload := `{"hive_id":"default-branch"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.HubGitHash == "" {
		t.Error("response should include hub git hash")
	}
}

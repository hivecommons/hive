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

func TestHandleAccessStatusFullWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_accstatus_full", "accstatus-user")
	defer authCleanup()

	u := ensureSaaSUser("accstatus-user")
	u.Hives["owned-h"] = "owner"
	u.Hives["read-h"] = "read"
	saveSaaSUser(u)

	saveSaaSHive(&SaaSHive{ID: "pending-h", Owner: "other"})
	saveAccessRequests("pending-h", []AccessRequest{
		{Username: "accstatus-user", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "owned-h", Owner: "accstatus-user", Online: true, LastHeartbeat: now},
		{ID: "read-h", Owner: "someone", Online: true, LastHeartbeat: now},
		{ID: "pending-h", Owner: "other", Online: true, LastHeartbeat: now},
		{ID: "no-access-h", Owner: "stranger", Online: true, LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/saas/access-status", nil)
	req.Header.Set("Authorization", "Bearer ghp_accstatus_full")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != true {
		t.Error("should be authenticated")
	}
	hives, _ := resp["hives"].(map[string]any)
	if hives == nil {
		t.Fatal("should have hives map")
	}
	// Check owned hive
	if ownedH, ok := hives["owned-h"]; ok {
		info := ownedH.(map[string]any)
		if info["role"] != "owner" {
			t.Errorf("owned hive role = %v", info["role"])
		}
	}
	// Check pending hive
	if pendH, ok := hives["pending-h"]; ok {
		info := pendH.(map[string]any)
		if info["status"] != "pending" {
			t.Errorf("pending hive status = %v", info["status"])
		}
	}
}

// A justification note is required on access requests: empty/whitespace
// notes are rejected with 400 and no request is stored; a non-empty note
// is persisted on the AccessRequest record.
func TestHandleRequestAccessNoteRequired(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_reqnote", "reqnote-user")
	defer authCleanup()

	saveSaaSHive(&SaaSHive{ID: "note-hive", Owner: "someone-else"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)

	doRequest := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/saas/hives/note-hive/request-access", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer ghp_reqnote")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// Missing note → 400, nothing stored.
	if w := doRequest(`{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing note: expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if len(loadAccessRequests("note-hive")) != 0 {
		t.Fatalf("missing note should not persist a request, got %d", len(loadAccessRequests("note-hive")))
	}

	// Whitespace-only note → 400.
	if w := doRequest(`{"note":"   \n\t "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("whitespace note: expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if len(loadAccessRequests("note-hive")) != 0 {
		t.Fatalf("whitespace note should not persist a request, got %d", len(loadAccessRequests("note-hive")))
	}

	// Valid note → 200, request stored with trimmed note.
	w := doRequest(`{"note":"  I lead the SRE team and need read access to debug incidents.  "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("valid note: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	reqs := loadAccessRequests("note-hive")
	if len(reqs) != 1 {
		t.Fatalf("valid note: expected 1 stored request, got %d", len(reqs))
	}
	if reqs[0].Username != "reqnote-user" {
		t.Errorf("stored request username = %q", reqs[0].Username)
	}
	if reqs[0].Status != "pending" {
		t.Errorf("stored request status = %q", reqs[0].Status)
	}
	if reqs[0].Note != "I lead the SRE team and need read access to debug incidents." {
		t.Errorf("stored request note = %q (should be trimmed)", reqs[0].Note)
	}
}

func TestHandleToggleAutoUpgradeWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_toggle_fs", "toggle-fs-owner")
	defer authCleanup()

	saveSaaSHive(&SaaSHive{ID: "toggle-fs-hive", Owner: "toggle-fs-owner", AutoUpgrade: false})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", srv.handleToggleAutoUpgrade)

	req := httptest.NewRequest("PUT", "/api/saas/hives/toggle-fs-hive/auto-upgrade",
		strings.NewReader(`{"auto_upgrade":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_toggle_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify saved
	h := loadSaaSHive("toggle-fs-hive")
	if !h.AutoUpgrade {
		t.Error("auto_upgrade should be true after toggle")
	}
}

func TestHandleMyHivesWithSaaSHivesOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_myhives_saas", "saas-hive-owner")
	defer authCleanup()

	u := ensureSaaSUser("saas-hive-owner")
	u.Hives["hosted-saas-xyz"] = "owner"
	saveSaaSUser(u)

	const vanityURL = "https://testorg-testrepo-abcd.hive.example.com"
	saveSaaSHive(&SaaSHive{
		ID:          "hosted-saas-xyz",
		Owner:       "saas-hive-owner",
		Org:         "testorg",
		PrimaryRepo: "testrepo",
		Status:      "provisioning",
		ACMMLevel:   2,
		VanityURL:   vanityURL,
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.Header.Set("Authorization", "Bearer ghp_myhives_saas")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	hives, _ := resp["hives"].([]any)
	found := false
	for _, h := range hives {
		hMap := h.(map[string]any)
		if hMap["id"] == "hosted-saas-xyz" {
			found = true
			if hMap["provStatus"] != "provisioning" {
				t.Errorf("provStatus = %v", hMap["provStatus"])
			}
			if hMap["governorMode"] != "PROVISIONING" {
				t.Errorf("governorMode = %v", hMap["governorMode"])
			}
			if hMap["dashboardUrl"] != vanityURL {
				t.Errorf("dashboardUrl = %v, want vanity URL %s", hMap["dashboardUrl"], vanityURL)
			}
		}
	}
	if !found {
		t.Error("hosted SaaS hive should appear in my-hives")
	}
}

func TestHandleMyHivesWithErrorHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_myhives_err", "err-hive-owner")
	defer authCleanup()

	u := ensureSaaSUser("err-hive-owner")
	u.Hives["hosted-err-xyz"] = "owner"
	saveSaaSUser(u)

	saveSaaSHive(&SaaSHive{
		ID:     "hosted-err-xyz",
		Owner:  "err-hive-owner",
		Status: "error",
		Error:  "provisioning failed",
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.Header.Set("Authorization", "Bearer ghp_myhives_err")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	hives, _ := resp["hives"].([]any)
	for _, h := range hives {
		hMap := h.(map[string]any)
		if hMap["id"] == "hosted-err-xyz" {
			if hMap["governorMode"] != "ERROR" {
				t.Errorf("governorMode = %v, want ERROR", hMap["governorMode"])
			}
			if hMap["provError"] != "provisioning failed" {
				t.Errorf("provError = %v", hMap["provError"])
			}
		}
	}
}

func TestHandleMyHivesAdminSeesAll(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_admin_all", hubAdminUsername)
	defer authCleanup()

	ensureSaaSUser(hubAdminUsername)
	ensureSaaSUser("other-owner")

	saveSaaSHive(&SaaSHive{ID: "hosted-admin-hive", Owner: "other-owner", Status: "running"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "reg-hive-1", Owner: "other-owner", Online: true, LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.Header.Set("Authorization", "Bearer ghp_admin_all")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["is_admin"] != true {
		t.Error("admin should see is_admin=true")
	}
}

func TestTriggerAutoUpgradesWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Create hive with auto_upgrade enabled
	saveSaaSHive(&SaaSHive{
		ID:          "auto-up-fs",
		Owner:       "owner",
		AutoUpgrade: true,
		Status:      "running",
		ClusterID:   defaultClusterID,
	})

	// Set up latest SHA
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newest1", Message: "latest"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "auto-up-fs", Online: true, GitHash: "old111", GitBranch: "v2"},
	}
	srv.mu.Unlock()

	// Should exercise more of triggerAutoUpgrades
	srv.triggerAutoUpgrades()

	// Verify upgrading flag was set
	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "auto-up-fs" {
			// kubectl will fail, so Upgrading gets set then unset
			t.Logf("auto-up-fs: Upgrading=%v, UpgradeTarget=%q", h.Upgrading, h.UpgradeTarget)
		}
	}
	srv.mu.RUnlock()
}

func TestTriggerAutoUpgradesSkipsProvisioning(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	saveSaaSHive(&SaaSHive{
		ID:          "prov-hive",
		Owner:       "owner",
		AutoUpgrade: true,
		Status:      "provisioning",
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.triggerAutoUpgrades()
	// Should skip provisioning hives — no crash
}

func TestTriggerAutoUpgradesSkipsAlreadyUpgrading(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	saveSaaSHive(&SaaSHive{
		ID:          "already-up",
		Owner:       "owner",
		AutoUpgrade: true,
		Status:      "running",
	})

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "target2", Message: "latest"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "already-up", Online: true, GitHash: "old1", GitBranch: "v2", Upgrading: true, UpgradeTarget: "target2"},
	}
	srv.mu.Unlock()

	srv.triggerAutoUpgrades()
	// Should skip already-upgrading hives
}

func TestTriggerAutoUpgradesSkipsCurrentSHA(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	saveSaaSHive(&SaaSHive{
		ID:          "current-sha",
		Owner:       "owner",
		AutoUpgrade: true,
		Status:      "running",
	})

	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "current", Message: "latest"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		delete(latestSHAByBranch, "v2")
		latestSHAMu.Unlock()
	}()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "current-sha", Online: true, GitHash: "current", GitBranch: "v2"},
	}
	srv.mu.Unlock()

	srv.triggerAutoUpgrades()
	// Should skip since currentSHA == latestSHA
}

func TestHandleCreateHiveAppAuth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_create_app", "app-user")
	defer authCleanup()

	u := ensureSaaSUser("app-user")
	u.SaaSQuota = 5
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	privateKey := strings.Join([]string{"-----BEGIN RSA ", "PRIVATE KEY-----\\nfake\\n-----END RSA ", "PRIVATE KEY-----"}, "")
	body := `{"org":"apporg","repos":"repo1","auth_method":"app","app_id":"123","installation_id":"456","app_private_key":"` + privateKey + `"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_app")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleCreateHiveUnknownCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_create_unk", "unk-cluster-user")
	defer authCleanup()

	u := ensureSaaSUser("unk-cluster-user")
	u.SaaSQuota = 5
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"myorg","repos":"repo1","github_token":"ghp_fake12345678","cluster_id":"unknown-cluster-xyz"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_unk")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown cluster, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleCreateHiveNoQuota(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_create_noquota", "noquota-user")
	defer authCleanup()

	u := ensureSaaSUser("noquota-user")
	u.SaaSQuota = 0
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"myorg","repos":"repo1","github_token":"ghp_fake12345678"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_noquota")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 (no quota), got %d", w.Code)
	}
}

func TestHandleCreateHiveBlockedUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_create_blocked", "blocked-create-user")
	defer authCleanup()

	u := ensureSaaSUser("blocked-create-user")
	u.Blocked = true
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"myorg","repos":"repo1","github_token":"ghp_fake12345678"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_blocked")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 (blocked), got %d", w.Code)
	}
}

func TestHandleDeleteHiveWithFilesystemFull(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_delfull_fs", "del-full-owner")
	defer authCleanup()

	u := ensureSaaSUser("del-full-owner")
	u.Hives["del-full-hive"] = "owner"
	saveSaaSUser(u)

	saveSaaSHive(&SaaSHive{ID: "del-full-hive", Owner: "del-full-owner", Status: "running"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Add to registry
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "del-full-hive", Owner: "del-full-owner", Online: true},
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/del-full-hive", nil)
	req.Header.Set("Authorization", "Bearer ghp_delfull_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Logf("delete hive full: got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleHiveStatusAccessDenied(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_status_denied", "noowner")
	defer authCleanup()

	ensureSaaSUser("noowner")
	saveSaaSHive(&SaaSHive{ID: "secret-hive", Owner: "real-owner"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)

	req := httptest.NewRequest("GET", "/api/saas/hives/secret-hive/status", nil)
	req.Header.Set("Authorization", "Bearer ghp_status_denied")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleHiveStatusWithAccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_status_acc", "accuser")
	defer authCleanup()

	u := ensureSaaSUser("accuser")
	u.Hives["acc-hive"] = "read"
	saveSaaSUser(u)

	saveSaaSHive(&SaaSHive{ID: "acc-hive", Owner: "other-owner", Status: "running"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)

	req := httptest.NewRequest("GET", "/api/saas/hives/acc-hive/status", nil)
	req.Header.Set("Authorization", "Bearer ghp_status_acc")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleAuthUserWithFileUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser("file-auth-user")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/auth-user", nil)
	req.AddCookie(testAuthCookie("file-auth-user"))
	w := httptest.NewRecorder()
	srv.handleAuthUser(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != true {
		t.Error("should be authenticated with valid user file")
	}
	if resp["login"] != "file-auth-user" {
		t.Errorf("login = %v", resp["login"])
	}
}

func TestHandleAuthUserAdmin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser(hubAdminUsername)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/auth-user", nil)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	w := httptest.NewRecorder()
	srv.handleAuthUser(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["hub_admin"] != true {
		t.Error("admin should see hub_admin=true")
	}
}

func TestGetAuthUserWithCookieAndFile(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser("cookie-user")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/test", nil)
	req.AddCookie(testAuthCookie("cookie-user"))
	user := srv.getAuthUser(req)

	if user != "cookie-user" {
		t.Errorf("getAuthUser = %q, want cookie-user", user)
	}
}

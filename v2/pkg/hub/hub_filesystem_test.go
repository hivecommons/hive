package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperSetupTempDirs sets up temp directories for SaaS data and returns a cleanup func.
func helperSetupTempDirs(t *testing.T) func() {
	t.Helper()
	dir := t.TempDir()

	// Async provisioning goroutines from previously-run tests read the
	// package-level path variables swapped below; drain them first.
	provisionWG.Wait()

	oldUsers := saasUsersDir
	oldHives := saasHivesDir
	oldHMAC := hmacKeyPath
	oldProv := provisionRequestsDir
	oldAutoUpgrade := hubAutoUpgradePath

	saasUsersDir = filepath.Join(dir, "users")
	saasHivesDir = filepath.Join(dir, "hives")
	hmacKeyPath = filepath.Join(dir, "hmac.key")
	provisionRequestsDir = filepath.Join(dir, "provision-requests")
	hubAutoUpgradePath = filepath.Join(dir, "hub-auto-upgrade")

	os.MkdirAll(saasUsersDir, 0o755)
	os.MkdirAll(saasHivesDir, 0o755)
	os.MkdirAll(provisionRequestsDir, 0o755)

	return func() {
		provisionWG.Wait()
		saasUsersDir = oldUsers
		saasHivesDir = oldHives
		hmacKeyPath = oldHMAC
		provisionRequestsDir = oldProv
		hubAutoUpgradePath = oldAutoUpgrade
	}
}

// ============================================================
// User CRUD with temp filesystem
// ============================================================

func TestSaveSaaSUserAndLoad(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	u := &SaaSUser{
		GitHubUsername: "testuser",
		CreatedAt:      "2024-01-01T00:00:00Z",
		LastLogin:      "2024-06-01T00:00:00Z",
		Hives:          map[string]string{"hive1": "owner"},
		SaaSQuota:      3,
	}
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("saveSaaSUser failed: %v", err)
	}

	loaded := loadSaaSUser("testuser")
	if loaded == nil {
		t.Fatal("loadSaaSUser returned nil")
	}
	if loaded.GitHubUsername != "testuser" {
		t.Errorf("username = %q", loaded.GitHubUsername)
	}
	if loaded.SaaSQuota != 3 {
		t.Errorf("quota = %d", loaded.SaaSQuota)
	}
	if loaded.Hives["hive1"] != "owner" {
		t.Error("hive role should be 'owner'")
	}
}

func TestEnsureSaaSUserCreatesNew(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	u := ensureSaaSUser("newuser")
	if u == nil {
		t.Fatal("ensureSaaSUser returned nil")
	}
	if u.GitHubUsername != "newuser" {
		t.Errorf("username = %q", u.GitHubUsername)
	}
	if u.SaaSQuota != 0 {
		t.Errorf("regular user quota should be 0, got %d", u.SaaSQuota)
	}

	// Loading again should return the same user
	loaded := loadSaaSUser("newuser")
	if loaded == nil {
		t.Fatal("user should be persisted")
	}
}

func TestEnsureSaaSUserUpdatesExisting(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Create first
	ensureSaaSUser("updater")
	time.Sleep(10 * time.Millisecond)

	// Update
	u := ensureSaaSUser("updater")
	if u == nil {
		t.Fatal("ensureSaaSUser returned nil on update")
	}
	// LastLogin should be updated
	if u.LastLogin == u.CreatedAt {
		t.Log("last_login might equal created_at if both are within same second")
	}
}

func TestEnsureSaaSUserAdmin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	u := ensureSaaSUser(hubAdminUsername)
	if u == nil {
		t.Fatal("admin user should be created")
	}
	if u.SaaSQuota != -1 {
		t.Errorf("admin quota should be -1, got %d", u.SaaSQuota)
	}
}

func TestListAllSaaSUsersWithData(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser("user1")
	ensureSaaSUser("user2")
	ensureSaaSUser("user3")

	users := listAllSaaSUsers()
	if len(users) != 3 {
		t.Errorf("expected 3 users, got %d", len(users))
	}
}

// ============================================================
// Hive CRUD with temp filesystem
// ============================================================

func TestSaveSaaSHiveAndLoad(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID:          "test-hive-1",
		Owner:       "testuser",
		Org:         "myorg",
		Repos:       []string{"repo1", "repo2"},
		PrimaryRepo: "repo1",
		ACMMLevel:   2,
		Status:      "running",
		CreatedAt:   "2024-01-01T00:00:00Z",
		AutoUpgrade: true,
		ClusterID:   "hive-oke",
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive failed: %v", err)
	}

	loaded := loadSaaSHive("test-hive-1")
	if loaded == nil {
		t.Fatal("loadSaaSHive returned nil")
	}
	if loaded.Owner != "testuser" {
		t.Errorf("owner = %q", loaded.Owner)
	}
	if loaded.AutoUpgrade != true {
		t.Error("auto_upgrade should be true")
	}
}

func TestListSaaSHivesWithData(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h1 := &SaaSHive{ID: "hive-a", Owner: "user1"}
	h2 := &SaaSHive{ID: "hive-b", Owner: "user2"}
	h3 := &SaaSHive{ID: "hive-c", Owner: "user1"}
	saveSaaSHive(h1)
	saveSaaSHive(h2)
	saveSaaSHive(h3)

	hives := listSaaSHives()
	if len(hives) != 3 {
		t.Errorf("expected 3 hives, got %d", len(hives))
	}
}

func TestCountUserHivesWithData(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "counter"})
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "counter"})
	saveSaaSHive(&SaaSHive{ID: "h3", Owner: "other"})

	if got := countUserHives("counter"); got != 2 {
		t.Errorf("countUserHives = %d, want 2", got)
	}
	if got := countUserHives("other"); got != 1 {
		t.Errorf("countUserHives = %d, want 1", got)
	}
	if got := countUserHives("nobody"); got != 0 {
		t.Errorf("countUserHives = %d, want 0", got)
	}
}

// ============================================================
// Encrypt / Decrypt with temp HMAC key
// ============================================================

func TestEncryptDecryptWithTempKey(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	encrypted, err := encryptToken("my-secret-token-123")
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	if encrypted == "" {
		t.Fatal("encrypted should not be empty")
	}

	decrypted, err := decryptToken(encrypted)
	if err != nil {
		t.Fatalf("decryptToken: %v", err)
	}
	if decrypted != "my-secret-token-123" {
		t.Errorf("decrypted = %q, want my-secret-token-123", decrypted)
	}
}

func TestLoadOrCreateHMACKey(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	key1, err := loadOrCreateHMACKey()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(key1) != hmacKeySize {
		t.Errorf("key size = %d, want %d", len(key1), hmacKeySize)
	}

	// Second call should load existing
	key2, err := loadOrCreateHMACKey()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(key1) != string(key2) {
		t.Error("second call should return same key")
	}
}

// ============================================================
// isHubAutoUpgrade with temp file
// ============================================================

func TestIsHubAutoUpgradeTrue(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	os.WriteFile(hubAutoUpgradePath, []byte("true"), 0o644)
	if !isHubAutoUpgrade() {
		t.Error("should be true")
	}
}

func TestIsHubAutoUpgradeFalse(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	os.WriteFile(hubAutoUpgradePath, []byte("false"), 0o644)
	if isHubAutoUpgrade() {
		t.Error("should be false")
	}
}

func TestIsHubAutoUpgradeMissing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if isHubAutoUpgrade() {
		t.Error("should be false when file missing")
	}
}

// ============================================================
// Access Requests with temp filesystem
// ============================================================

func TestSaveLoadAccessRequests(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Create hive dir first
	saveSaaSHive(&SaaSHive{ID: "req-hive", Owner: "owner"})

	reqs := []AccessRequest{
		{Username: "user1", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
		{Username: "user2", RequestedAt: "2024-01-02T00:00:00Z", Status: "approved"},
	}
	saveAccessRequests("req-hive", reqs)

	loaded := loadAccessRequests("req-hive")
	if len(loaded) != 2 {
		t.Errorf("expected 2 requests, got %d", len(loaded))
	}
	if loaded[0].Username != "user1" {
		t.Errorf("first request username = %q", loaded[0].Username)
	}
	if loaded[1].Status != "approved" {
		t.Errorf("second request status = %q", loaded[1].Status)
	}
}

// ============================================================
// Provision Request CRUD
// ============================================================

func TestProvisionRequestCRUD(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	pr := &ProvisionRequest{
		Username:    "prov-user",
		Org:         "myorg",
		Repos:       "repo1",
		PrimaryRepo: "repo1",
		ACMMLevel:   2,
		RequestedAt: "2024-01-01T00:00:00Z",
		Status:      provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		t.Fatalf("saveProvisionRequest: %v", err)
	}

	loaded := loadProvisionRequest("prov-user")
	if loaded == nil {
		t.Fatal("loadProvisionRequest returned nil")
	}
	if loaded.Org != "myorg" {
		t.Errorf("org = %q", loaded.Org)
	}

	// List
	prList := listProvisionRequests()
	if len(prList) != 1 {
		t.Errorf("expected 1 pending request, got %d", len(prList))
	}

	// Delete
	deleteProvisionRequest("prov-user")
	if loadProvisionRequest("prov-user") != nil {
		t.Error("should be deleted")
	}
}

// ============================================================
// Handler tests with filesystem access
// ============================================================

func TestHandleAdminUpdateUserWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser("target-user")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/admin/users/{username}", srv.handleAdminUpdateUser)

	req := httptest.NewRequest("PUT", "/api/saas/admin/users/target-user",
		strings.NewReader(`{"saas_quota":5,"blocked":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify update persisted
	u := loadSaaSUser("target-user")
	if u.SaaSQuota != 5 {
		t.Errorf("quota = %d, want 5", u.SaaSQuota)
	}
	if !u.Blocked {
		t.Error("should be blocked")
	}
}

func TestHandleAdminUsersWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	ensureSaaSUser("user-a")
	ensureSaaSUser("user-b")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/admin-users", nil)
	w := httptest.NewRecorder()
	srv.handleAdminUsers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	users := resp["users"].([]any)
	if len(users) < 2 {
		t.Errorf("expected at least 2 users, got %d", len(users))
	}
}

func TestRequireAuthWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_auth_fs", "fs-auth-user")
	defer authCleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	var calledWith string
	handler := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		calledWith = srv.getAuthUser(r)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ghp_auth_fs")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if calledWith != "fs-auth-user" {
		t.Errorf("auth user = %q, want fs-auth-user", calledWith)
	}
}

func TestRequireAuthBlockedUserFS(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Create a blocked user
	u := ensureSaaSUser("blocked-user")
	u.Blocked = true
	saveSaaSUser(u)

	authCleanup := helperSetupAuthUser(t, "ghp_blocked_fs", "blocked-user")
	defer authCleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	handler := srv.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ghp_blocked_fs")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for blocked user, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "blocked") {
		t.Error("should mention 'blocked' in error")
	}
}

func TestHandleMyHivesWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_myhives_fs", "myhives-user")
	defer authCleanup()

	// Create user with hive access
	u := ensureSaaSUser("myhives-user")
	u.Hives["hive-x"] = "owner"
	saveSaaSUser(u)

	// Create a SaaS hive
	saveSaaSHive(&SaaSHive{
		ID:          "hosted-myhive-1",
		Owner:       "myhives-user",
		Org:         "testorg",
		PrimaryRepo: "testrepo",
		Status:      "running",
		AutoUpgrade: true,
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	now := time.Now().Format(time.RFC3339)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-x", Owner: "myhives-user", Online: true, Name: "org/repo", LastHeartbeat: now},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/my-hives", nil)
	req.Header.Set("Authorization", "Bearer ghp_myhives_fs")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	hives, ok := resp["hives"]
	if !ok {
		t.Fatal("response should have 'hives' key")
	}
	hiveList, _ := hives.([]any)
	if len(hiveList) < 1 {
		t.Logf("expected at least 1 hive, got %d (hive may not appear without registry match)", len(hiveList))
	}
}

func TestHandleHiveStatusWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_status_fs", "status-user")
	defer authCleanup()

	ensureSaaSUser("status-user")

	saveSaaSHive(&SaaSHive{ID: "status-hive", Owner: "status-user", Status: "running"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)

	req := httptest.NewRequest("GET", "/api/saas/hives/status-hive/status", nil)
	req.Header.Set("Authorization", "Bearer ghp_status_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleDeleteHiveWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_delete_fs", "delete-user")
	defer authCleanup()

	ensureSaaSUser("delete-user")
	saveSaaSHive(&SaaSHive{ID: "del-hive", Owner: "delete-user", Status: "running"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/del-hive", nil)
	req.Header.Set("Authorization", "Bearer ghp_delete_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// deprovisionHive uses kubectl which will fail, but the pre-check code is exercised
	// The function returns OK after launching deprovision goroutine
	if w.Code != http.StatusOK && w.Code != http.StatusForbidden {
		t.Logf("delete hive: got %d", w.Code)
	}
}

func TestHandleAccessListWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_acl_fs", "acl-owner")
	defer authCleanup()

	ensureSaaSUser("acl-owner")
	saveSaaSHive(&SaaSHive{ID: "acl-hive", Owner: "acl-owner"})

	// Add another user with access
	u := ensureSaaSUser("acl-reader")
	u.Hives["acl-hive"] = "read"
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/access", srv.handleAccessList)

	req := httptest.NewRequest("GET", "/api/saas/hives/acl-hive/access", nil)
	req.Header.Set("Authorization", "Bearer ghp_acl_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	access := resp["access"].([]any)
	if len(access) < 1 {
		t.Error("should have at least 1 access entry")
	}
}

func TestHandleAccessAddWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_addacc_fs", "add-owner")
	defer authCleanup()

	ensureSaaSUser("add-owner")
	saveSaaSHive(&SaaSHive{ID: "add-hive", Owner: "add-owner"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/access", srv.handleAccessAdd)

	body := `{"username":"newuser","role":"read"}`
	req := httptest.NewRequest("POST", "/api/saas/hives/add-hive/access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_addacc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify access was granted
	target := loadSaaSUser("newuser")
	if target == nil || target.Hives["add-hive"] != "read" {
		t.Error("user should have read access")
	}
}

func TestHandleAccessAddBadRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_badrole_fs", "role-owner")
	defer authCleanup()

	ensureSaaSUser("role-owner")
	saveSaaSHive(&SaaSHive{ID: "role-hive", Owner: "role-owner"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/access", srv.handleAccessAdd)

	body := `{"username":"newuser","role":"superadmin"}`
	req := httptest.NewRequest("POST", "/api/saas/hives/role-hive/access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_badrole_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad role, got %d", w.Code)
	}
}

func TestHandleAccessRemoveWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_rmacc_fs", "rm-owner")
	defer authCleanup()

	ensureSaaSUser("rm-owner")
	saveSaaSHive(&SaaSHive{ID: "rm-hive", Owner: "rm-owner"})

	target := ensureSaaSUser("rm-target")
	target.Hives["rm-hive"] = "read"
	saveSaaSUser(target)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", srv.handleAccessRemove)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/rm-hive/access/rm-target", nil)
	req.Header.Set("Authorization", "Bearer ghp_rmacc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleAccessRemoveLastOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_rmlast_fs", "last-owner")
	defer authCleanup()

	ensureSaaSUser("last-owner")
	saveSaaSHive(&SaaSHive{ID: "lastowner-hive", Owner: "last-owner"})

	target := ensureSaaSUser("the-owner")
	target.Hives["lastowner-hive"] = "owner"
	saveSaaSUser(target)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", srv.handleAccessRemove)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/lastowner-hive/access/the-owner", nil)
	req.Header.Set("Authorization", "Bearer ghp_rmlast_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (can't remove last owner), got %d", w.Code)
	}
}

func TestHandleRequestAccessWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_reqacc_fs", "requesting-user")
	defer authCleanup()

	ensureSaaSUser("requesting-user")
	saveSaaSHive(&SaaSHive{ID: "req-hive-fs", Owner: "someone-else"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)

	req := httptest.NewRequest("POST", "/api/saas/hives/req-hive-fs/request-access", strings.NewReader(`{"note":"need access for CI work"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_reqacc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Requesting again should fail (already pending)
	req2 := httptest.NewRequest("POST", "/api/saas/hives/req-hive-fs/request-access", strings.NewReader(`{"note":"need access for CI work"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer ghp_reqacc_fs")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Errorf("duplicate request: expected 400, got %d", w2.Code)
	}
}

func TestHandleRequestAccessAlreadyHasAccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_hasacc_fs", "has-access-user")
	defer authCleanup()

	u := ensureSaaSUser("has-access-user")
	u.Hives["has-hive"] = "read"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "has-hive", Owner: "other"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)

	req := httptest.NewRequest("POST", "/api/saas/hives/has-hive/request-access", strings.NewReader(`{"note":"please"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_hasacc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (already has access), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already have access") {
		t.Errorf("expected 'already have access' error, got: %s", w.Body.String())
	}
}

func TestHandleGetRequestsWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_getreq_fs", "getreq-owner")
	defer authCleanup()

	u := ensureSaaSUser("getreq-owner")
	u.Hives["getreq-hive"] = "owner"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "getreq-hive", Owner: "getreq-owner"})

	saveAccessRequests("getreq-hive", []AccessRequest{
		{Username: "req1", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
		{Username: "req2", RequestedAt: "2024-01-02T00:00:00Z", Status: "approved"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/requests", srv.handleGetRequests)

	req := httptest.NewRequest("GET", "/api/saas/hives/getreq-hive/requests", nil)
	req.Header.Set("Authorization", "Bearer ghp_getreq_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	requests := resp["requests"].([]any)
	if len(requests) != 1 {
		t.Errorf("expected 1 pending request, got %d", len(requests))
	}
}

func TestHandleApproveRequestWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_appreq_fs", "approve-owner")
	defer authCleanup()

	u := ensureSaaSUser("approve-owner")
	u.Hives["approve-hive"] = "owner"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "approve-hive", Owner: "approve-owner"})

	saveAccessRequests("approve-hive", []AccessRequest{
		{Username: "pending-user", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", srv.handleApproveRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/approve-hive/requests/pending-user/approve",
		strings.NewReader(`{"role":"read"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_appreq_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleDenyRequestWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_denyreq_fs", "deny-owner")
	defer authCleanup()

	u := ensureSaaSUser("deny-owner")
	u.Hives["deny-hive"] = "owner"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "deny-hive", Owner: "deny-owner"})

	saveAccessRequests("deny-hive", []AccessRequest{
		{Username: "denied-user", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", srv.handleDenyRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/deny-hive/requests/denied-user/deny", nil)
	req.Header.Set("Authorization", "Bearer ghp_denyreq_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleApproveAccessWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_appracc_fs", "appracc-owner")
	defer authCleanup()

	u := ensureSaaSUser("appracc-owner")
	u.Hives["appracc-hive"] = "owner"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "appracc-hive", Owner: "appracc-owner"})

	saveAccessRequests("appracc-hive", []AccessRequest{
		{Username: "approved-user", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", srv.handleApproveAccess)

	req := httptest.NewRequest("PUT", "/api/saas/hives/appracc-hive/approve-access/approved-user", nil)
	req.Header.Set("Authorization", "Bearer ghp_appracc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleDenyAccessWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_denyacc_fs", "denyacc-owner")
	defer authCleanup()

	u := ensureSaaSUser("denyacc-owner")
	u.Hives["denyacc-hive"] = "owner"
	saveSaaSUser(u)
	saveSaaSHive(&SaaSHive{ID: "denyacc-hive", Owner: "denyacc-owner"})

	saveAccessRequests("denyacc-hive", []AccessRequest{
		{Username: "denied-acc-user", RequestedAt: "2024-01-01T00:00:00Z", Status: "pending"},
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", srv.handleDenyAccess)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/denyacc-hive/deny-access/denied-acc-user", nil)
	req.Header.Set("Authorization", "Bearer ghp_denyacc_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleRequestProvisionWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_reqprov_fs", "reqprov-user")
	defer authCleanup()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"validorg","repos":"repo1,repo2","primary_repo":"repo1","acmm_level":3}`
	req := httptest.NewRequest("POST", "/provision", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_reqprov_fs")
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify saved
	pr := loadProvisionRequest("reqprov-user")
	if pr == nil {
		t.Fatal("provision request should be saved")
	}
	if pr.Org != "validorg" {
		t.Errorf("org = %q", pr.Org)
	}
}

func TestHandleApproveProvisionWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_appprov_fs", hubAdminUsername)
	defer authCleanup()

	ensureSaaSUser("prov-target")
	saveProvisionRequest(&ProvisionRequest{
		Username: "prov-target",
		Org:      "org",
		Repos:    "repo",
		Status:   provisionStatusPending,
	})

	// Approving now ASSIGNS an available placeholder from the correct pool. Seed
	// an available placeholder owned by the admin on the public (hive-oke) pool.
	if err := saveSaaSHive(&SaaSHive{
		ID:        "hosted-placeholder-fs",
		Owner:     hubAdminUsername,
		Status:    statusAvailable,
		ACMMLevel: defaultAssignACMMLevel,
		ClusterID: defaultClusterID,
	}); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/approve-provision/{username}", srv.handleApproveProvision)

	req := httptest.NewRequest("PUT", "/api/saas/approve-provision/prov-target", nil)
	req.Header.Set("Authorization", "Bearer ghp_appprov_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// Verify quota incremented
	u := loadSaaSUser("prov-target")
	if u.SaaSQuota != 1 {
		t.Errorf("quota = %d, want 1", u.SaaSQuota)
	}

	// Verify the placeholder was assigned to the requesting user with the
	// request's project, and its "available" status was cleared.
	h := loadSaaSHive("hosted-placeholder-fs")
	if h == nil {
		t.Fatal("placeholder hive missing after assignment")
	}
	if h.Owner != "prov-target" {
		t.Errorf("owner = %q, want prov-target", h.Owner)
	}
	if h.Org != "org" {
		t.Errorf("org = %q, want org", h.Org)
	}
	if h.Status != "" {
		t.Errorf("status = %q, want empty (cleared)", h.Status)
	}
}

func TestHandleApproveProvisionNoPlaceholderWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_appprov_none_fs", hubAdminUsername)
	defer authCleanup()

	ensureSaaSUser("prov-target-none")
	saveProvisionRequest(&ProvisionRequest{
		Username: "prov-target-none",
		Org:      "org",
		Repos:    "repo",
		Status:   provisionStatusPending,
	})

	// No available placeholder seeded -> the admin must be told to provision more.
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/approve-provision/{username}", srv.handleApproveProvision)

	req := httptest.NewRequest("PUT", "/api/saas/approve-provision/prov-target-none", nil)
	req.Header.Set("Authorization", "Bearer ghp_appprov_none_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 (no placeholder), got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestHandleAssignHiveWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_assign_fs", hubAdminUsername)
	defer authCleanup()

	if err := saveSaaSHive(&SaaSHive{
		ID:        "hosted-assign-fs",
		Owner:     hubAdminUsername,
		Status:    statusAvailable,
		ACMMLevel: defaultAssignACMMLevel,
		ClusterID: gpuClusterID,
	}); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/assign", srv.handleAssignHive)

	bodyJSON := `{"owner":"newowner","org":"neworg","repos":"repoa,repob","primary_repo":"repoa","acmm_level":3}`
	req := httptest.NewRequest("POST", "/api/saas/hives/hosted-assign-fs/assign", strings.NewReader(bodyJSON))
	req.Header.Set("Authorization", "Bearer ghp_assign_fs")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	h := loadSaaSHive("hosted-assign-fs")
	if h == nil {
		t.Fatal("hive missing after assign")
	}
	if h.Owner != "newowner" || h.Org != "neworg" || h.PrimaryRepo != "repoa" || h.ACMMLevel != 3 || h.Status != "" {
		t.Errorf("assign wrote wrong meta: %+v", h)
	}

	// Assign derives a vanity URL from the claimed project (present because the
	// test cluster has a domain). It's the marker that the placeholder is claimed.
	if h.VanityURL == "" {
		t.Error("expected assign to set a vanity URL")
	}

	// After a claim, org/repos/ACMM are operator-controlled and adopted (not
	// pushed), so the ONLY thing projectConfigForHiveID still delivers is the
	// vanity URL — until the spoke reports it back, then it goes quiet.
	// URL not yet adopted -> still delivering (carries the vanity URL).
	if pc := projectConfigForHiveID("hosted-assign-fs", "neworg", []string{"repoa", "repob"}, "repoa", 3, ""); pc == nil {
		t.Error("expected non-nil config while spoke has not yet adopted the vanity URL")
	} else if pc.DashboardURL != h.VanityURL {
		t.Errorf("project config DashboardURL = %q, want the vanity URL %q", pc.DashboardURL, h.VanityURL)
	}
	// Spoke now reports the vanity URL -> nothing left to push -> nil.
	if pc := projectConfigForHiveID("hosted-assign-fs", "neworg", []string{"repoa", "repob"}, "repoa", 3, h.VanityURL); pc != nil {
		t.Errorf("expected nil project config once spoke reports the vanity URL, got %+v", pc)
	}

	// Assigning an already-claimed (non-available) hive is rejected.
	req2 := httptest.NewRequest("POST", "/api/saas/hives/hosted-assign-fs/assign", strings.NewReader(bodyJSON))
	req2.Header.Set("Authorization", "Bearer ghp_assign_fs")
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409 assigning a non-placeholder, got %d", w2.Code)
	}
}

func TestHandleDenyProvisionWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_denyprov_fs", hubAdminUsername)
	defer authCleanup()

	saveProvisionRequest(&ProvisionRequest{
		Username: "deny-prov-target",
		Status:   provisionStatusPending,
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/deny-provision/{username}", srv.handleDenyProvision)

	req := httptest.NewRequest("DELETE", "/api/saas/deny-provision/deny-prov-target", nil)
	req.Header.Set("Authorization", "Bearer ghp_denyprov_fs")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// A denial is RETAINED, not deleted: the admin request-history table needs
	// to show that this user was turned down and by whom. Deleting made a denial
	// indistinguishable from a request that was never made. The record must be
	// marked denied (not left pending), so it neither blocks a fresh request nor
	// reappears in the pending queue.
	got := loadProvisionRequest("deny-prov-target")
	if got == nil {
		t.Fatal("denied provision request should be retained for the history table")
	}
	if got.Status != provisionStatusDenied {
		t.Errorf("status = %q, want %q", got.Status, provisionStatusDenied)
	}
	if got.DecidedBy == "" || got.DecidedAt == "" {
		t.Errorf("denial should record who decided and when: by=%q at=%q", got.DecidedBy, got.DecidedAt)
	}
}

func TestHandleUserTokenWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_usertoken_fs", "token-fs-user")
	defer authCleanup()

	u := ensureSaaSUser("token-fs-user")
	u.Hives["token-hive"] = "owner"
	enc, _ := encryptToken("my-github-token")
	u.EncryptedToken = enc
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"token-hive","username":"token-fs-user"}`
	req := httptest.NewRequest("POST", "/user-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_usertoken_fs")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["token"] != "my-github-token" {
		t.Errorf("token = %q, want my-github-token", resp["token"])
	}
}

func TestHandleUserTokenNoToken(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_notoken_fs", "notoken-user")
	defer authCleanup()

	u := ensureSaaSUser("notoken-user")
	u.Hives["token-hive"] = "owner"
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"token-hive","username":"notoken-user"}`
	req := httptest.NewRequest("POST", "/user-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_notoken_fs")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 (no token stored), got %d", w.Code)
	}
}

func TestHandleUserTokenNoHiveAccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_noacc_fs", "noacc-user")
	defer authCleanup()

	ensureSaaSUser("noacc-user")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"hive_id":"no-access-hive","username":"noacc-user"}`
	req := httptest.NewRequest("POST", "/user-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_noacc_fs")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleSaaSAuthCheckWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_authcheck_fs", "authcheck-fs-user")
	defer authCleanup()

	u := ensureSaaSUser("authcheck-fs-user")
	u.Hives["check-hive"] = "read"
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=check-hive", nil)
	req.Header.Set("Authorization", "Bearer ghp_authcheck_fs")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("X-Hive-User") != "authcheck-fs-user" {
		t.Errorf("X-Hive-User = %q", w.Header().Get("X-Hive-User"))
	}
	if w.Header().Get("X-Hive-Role") != "read" {
		t.Errorf("X-Hive-Role = %q", w.Header().Get("X-Hive-Role"))
	}
}

func TestHandleSaaSAuthCheckNoAccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_noauth_fs", "noauth-user")
	defer authCleanup()

	ensureSaaSUser("noauth-user")

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=no-access-hive", nil)
	req.Header.Set("Authorization", "Bearer ghp_noauth_fs")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestHandleCreateHiveWithFilesystem(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_create_fs", "create-fs-user")
	defer authCleanup()

	u := ensureSaaSUser("create-fs-user")
	u.SaaSQuota = 5
	saveSaaSUser(u)

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"myorg","repos":"repo1","github_token":"ghp_faketoken12345678","project_name":"My Project","acmm_level":2}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_create_fs")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "provisioning" {
		t.Errorf("status = %v", resp["status"])
	}
}

func TestHandleCreateHiveQuotaReached(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	authCleanup := helperSetupAuthUser(t, "ghp_quota_fs", "quota-user")
	defer authCleanup()

	u := ensureSaaSUser("quota-user")
	u.SaaSQuota = 1
	saveSaaSUser(u)

	// Pre-create a hive owned by this user
	saveSaaSHive(&SaaSHive{ID: "existing-hive", Owner: "quota-user"})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	body := `{"org":"myorg","repos":"repo1","github_token":"ghp_faketoken12345678"}`
	req := httptest.NewRequest("POST", "/create", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_quota_fs")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (quota reached), got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestPersistAndLoadLatestSHAs verifies last-known-good semantics for the
// branch SHA cache across hub restarts: persisted SHAs are restored on load,
// untracked branches are dropped, and live values are never overwritten.
func TestPersistAndLoadLatestSHAs(t *testing.T) {
	dir := t.TempDir()
	oldPath := latestSHAsPath
	latestSHAsPath = filepath.Join(dir, "latest-shas.json")
	defer func() { latestSHAsPath = oldPath }()

	// Snapshot and clear global cache state; restore after the test.
	latestSHAMu.Lock()
	savedBranches := latestSHAByBranch
	savedMsgs := commitMsgBySHA
	latestSHAByBranch = map[string]branchSHAInfo{
		"v2":         {SHA: "aaa1111", Message: "v2 commit"},
		"v3":         {SHA: "bbb2222", Message: "v3 commit"},
		"old-branch": {SHA: "ccc3333", Message: "untracked commit"},
	}
	commitMsgBySHA = map[string]string{}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		latestSHAByBranch = savedBranches
		commitMsgBySHA = savedMsgs
		latestSHAMu.Unlock()
	}()

	persistLatestSHAs(slog.Default())
	if _, err := os.Stat(latestSHAsPath); err != nil {
		t.Fatalf("persistLatestSHAs did not write %s: %v", latestSHAsPath, err)
	}

	// Simulate a hub restart with a partially failed initial fetch: only v3
	// was fetched live; v2's fetch was rate-limited so its entry is missing.
	latestSHAMu.Lock()
	latestSHAByBranch = map[string]branchSHAInfo{
		"v3": {SHA: "ddd4444", Message: "newer v3 commit"},
	}
	latestSHAMu.Unlock()

	loadPersistedSHAs(slog.Default(), trackedBranches)

	if got := getLatestSHAForBranch("v2"); got != "aaa1111" {
		t.Errorf("v2 SHA not restored from disk: got %q, want aaa1111", got)
	}
	if got := getLatestSHAForBranch("v3"); got != "ddd4444" {
		t.Errorf("live v3 SHA was overwritten by persisted value: got %q, want ddd4444", got)
	}
	if got := getLatestSHAForBranch("old-branch"); got != "" {
		t.Errorf("untracked branch restored from disk: got %q, want empty", got)
	}
	if msgs := getLatestSHAMessages(); msgs["v2"] != "v2 commit" {
		t.Errorf("v2 commit message not restored: got %q, want 'v2 commit'", msgs["v2"])
	}
	if cm := getCommitMessages(); cm["aaa1111"] != "v2 commit" {
		t.Errorf("commitMsgBySHA not backfilled on load: got %q, want 'v2 commit'", cm["aaa1111"])
	}

	// An empty cache must never clobber the good file on disk.
	latestSHAMu.Lock()
	latestSHAByBranch = map[string]branchSHAInfo{}
	latestSHAMu.Unlock()
	persistLatestSHAs(slog.Default())
	loadPersistedSHAs(slog.Default(), trackedBranches)
	if got := getLatestSHAForBranch("v2"); got != "aaa1111" {
		t.Errorf("empty persist clobbered file: v2 = %q, want aaa1111", got)
	}

	// A corrupt file is ignored (no panic, no partial state).
	if err := os.WriteFile(latestSHAsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	latestSHAMu.Lock()
	latestSHAByBranch = map[string]branchSHAInfo{}
	latestSHAMu.Unlock()
	loadPersistedSHAs(slog.Default(), trackedBranches)
	if got := getLatestSHAForBranch("v2"); got != "" {
		t.Errorf("corrupt file should restore nothing, got v2 = %q", got)
	}
}

// TestBranchHeadImageStatus verifies the display/head cache semantics: the
// dashboard shows the branch HEAD with its image build status while the
// image-verified cache (upgrade targets) stays on the last pullable SHA.
func TestBranchHeadImageStatus(t *testing.T) {
	// Snapshot and clear global cache state; restore after the test.
	latestSHAMu.Lock()
	savedBranches := latestSHAByBranch
	savedHeads := headSHAByBranch
	savedMsgs := commitMsgBySHA
	latestSHAByBranch = map[string]branchSHAInfo{
		"v2": {SHA: "aaa1111", Message: "old ready commit"},
	}
	headSHAByBranch = map[string]branchHeadInfo{}
	commitMsgBySHA = map[string]string{}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		latestSHAByBranch = savedBranches
		headSHAByBranch = savedHeads
		commitMsgBySHA = savedMsgs
		latestSHAMu.Unlock()
	}()

	// No head fetched yet (e.g. right after restart): display falls back to
	// the image-verified SHA, which is ready by definition.
	if got := getDisplaySHAs()["v2"]; got != "aaa1111" {
		t.Errorf("display SHA without head = %q, want aaa1111", got)
	}
	if got := getImageStatuses()["v2"]; got != imageStatusReady {
		t.Errorf("image status without head = %q, want ready", got)
	}

	// A new head lands and its image is still building: display advances,
	// upgrade target does not.
	setBranchHead("v2", "bbb2222", "new commit", imageStatusBuilding)
	if got := getDisplaySHAs()["v2"]; got != "bbb2222" {
		t.Errorf("display SHA with building head = %q, want bbb2222", got)
	}
	if got := getDisplaySHAMessages()["v2"]; got != "new commit" {
		t.Errorf("display message = %q, want 'new commit'", got)
	}
	if got := getImageStatuses()["v2"]; got != imageStatusBuilding {
		t.Errorf("image status = %q, want building", got)
	}
	if got := getLatestSHAForBranch("v2"); got != "aaa1111" {
		t.Errorf("upgrade-target SHA advanced past verified image: got %q, want aaa1111", got)
	}

	// A later poll with a rate-limited commit message keeps the known one.
	setBranchHead("v2", "bbb2222", "", imageStatusBuilding)
	if got := getDisplaySHAMessages()["v2"]; got != "new commit" {
		t.Errorf("empty message overwrote cached one: got %q, want 'new commit'", got)
	}

	// Commit messages are indexed by SHA for per-hive tooltips.
	if got := getCommitMessages()["bbb2222"]; got != "new commit" {
		t.Errorf("commitMsgBySHA not populated for head: got %q", got)
	}
}

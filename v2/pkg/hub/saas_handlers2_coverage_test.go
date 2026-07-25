package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================
// saas.go — more hive management handler paths (success/branches)
// ============================================================

func newHandlerHub2() *HubServer {
	return &HubServer{
		logger:                  slog.Default(),
		saveCh:                  make(chan struct{}, 1),
		hubBanners:              make(map[string]*HubBannerEntry),
		heartbeatSwitchTag:      make(map[string]string),
		heartbeatUpgrade:        make(map[string]string),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		clusters:                map[string]ClusterConfig{"hive-oke": *dynamicCluster()},
	}
}

func TestHandleCreateHiveSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// User with quota.
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{}, SaaSQuota: 5})
	s := newHandlerHub2()

	body := `{"org":"acme","repos":"repo1","primary_repo":"repo1","acmm_level":2,"cluster_id":"hive-oke","github_token":"ghp_testtoken"}`
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/api/saas/hives", body, "alice")
	s.handleCreateHive(rec, req)
	// The provision runs async; the handler itself should respond 200/accepted.
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body.String())
	}
	provisionWG.Wait()
}

func TestHandleCreateHiveNoQuota_Cov2(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{}, SaaSQuota: 0})
	s := newHandlerHub2()

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/api/saas/hives", `{"org":"a","repos":"r","github_token":"ghp_x"}`, "alice")
	s.handleCreateHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("no-quota status = %d, want 403", rec.Code)
	}
}

func TestHandleCreateHiveValidation_Cov2(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{}, SaaSQuota: 5})
	s := newHandlerHub2()

	cases := []struct {
		name, body string
		want       int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"missing org", `{"repos":"r","github_token":"ghp_x"}`, http.StatusBadRequest},
		{"invalid org", `{"org":"bad org!","repos":"r","github_token":"ghp_x"}`, http.StatusBadRequest},
		{"no creds", `{"org":"acme","repos":"repo"}`, http.StatusBadRequest},
		{"bad token prefix", `{"org":"acme","repos":"repo","github_token":"xxx"}`, http.StatusBadRequest},
		{"unknown cluster", `{"org":"acme","repos":"repo","github_token":"ghp_x","cluster_id":"nope"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := reqWithUser(http.MethodPost, "/api/saas/hives", tc.body, "alice")
			s.handleCreateHive(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHandleCreateHiveUnauthenticated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub2()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreateHive(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon create status = %d, want 401", rec.Code)
	}
}

func TestHandleDeleteHiveSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	s := newHandlerHub2()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Owner: "alice"}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodDelete, "/del", "", "alice"), "id", "h1")
	s.handleDeleteHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("delete status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleDeleteHiveGoneStillCleansRegistry(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	s := newHandlerHub2()
	s.registry.Hives = []RegistryEntry{{ID: "gone", Owner: "alice"}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodDelete, "/del", "", "alice"), "id", "gone")
	s.handleDeleteHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("delete-gone status = %d, want 200", rec.Code)
	}
}

func TestHandleDeleteHiveNotOwner_Cov2(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "bob", ClusterID: "hive-oke"})
	s := newHandlerHub2()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodDelete, "/del", "", "alice"), "id", "h1")
	s.handleDeleteHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner delete status = %d, want 403", rec.Code)
	}
}

func TestHandleMigrateHiveSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 2, ClusterID: "hive-oke"})
	s := newHandlerHub2()
	s.clusters["target"] = ClusterConfig{ID: "target", Name: "Target", InCluster: true, StorageType: storageTypeDynamic, Domain: "t.example.com"}
	s.registry.Hives = []RegistryEntry{{ID: "h1", ClusterID: "hive-oke"}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/mig", `{"target_cluster_id":"target"}`, "alice"), "id", "h1")
	s.handleMigrateHive(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Errorf("migrate status = %d, want 200/202 (body=%s)", rec.Code, rec.Body.String())
	}
	provisionWG.Wait()
}

func TestHandleAssignHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newHandlerHub2()

	// Non-admin -> 403.
	mkUser(t, "alice")
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/assign", `{}`, "alice"), "id", "ph1")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin assign status = %d, want 403", rec.Code)
	}

	// Admin, hive not found -> 404.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", `{"owner":"bob","org":"acme","repos":"r"}`, hubAdminUsername), "id", "missing")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing hive assign status = %d, want 404", rec.Code)
	}

	// Admin, hive not a placeholder -> 409.
	saveSaaSHive(&SaaSHive{ID: "claimed", Owner: "x", Status: "running"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", `{"owner":"bob","org":"acme","repos":"r"}`, hubAdminUsername), "id", "claimed")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("non-placeholder assign status = %d, want 409", rec.Code)
	}

	// Admin, available placeholder, valid assignment -> success.
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
	body := `{"owner":"bob","org":"acme","repos":"repo1,repo2","primary_repo":"repo1","acmm_level":3,"project_name":"Acme"}`
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", body, hubAdminUsername), "id", "ph1")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign success status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := loadSaaSHive("ph1")
	if got == nil || got.Owner != "bob" || got.Org != "acme" {
		t.Errorf("assignment not persisted: %+v", got)
	}

	// With GitHub App creds delivered via heartbeat.
	saveSaaSHive(&SaaSHive{ID: "ph2", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
	appBody := `{"owner":"bob","org":"acme","repos":"r","app_id":"123","installation_id":"456","app_private_key":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----"}`
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", appBody, hubAdminUsername), "id", "ph2")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign with app creds status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.pendingGitHubAppConfigs["ph2"]; !ok {
		t.Error("expected pending GitHub App config queued for ph2")
	}
}

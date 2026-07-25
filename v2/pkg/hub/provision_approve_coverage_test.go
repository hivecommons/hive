package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================
// saas.go — provision-approve flow + toggle-auto-upgrade heartbeat-only path
// server.go — loadRegistry
// ============================================================

func TestLoadRegistry(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> starts fresh, no error.
	old := registryPath
	registryPath = filepath.Join(dir, "missing.json")
	defer func() { registryPath = old }()
	s := &HubServer{logger: slog.Default()}
	s.loadRegistry()

	// Valid file -> loads hives.
	registryPath = filepath.Join(dir, "reg.json")
	os.WriteFile(registryPath, []byte(`{"hives":[{"id":"h1"},{"id":"h2"}]}`), 0o644)
	s2 := &HubServer{logger: slog.Default()}
	s2.loadRegistry()
	if len(s2.registry.Hives) != 2 {
		t.Errorf("expected 2 hives loaded, got %d", len(s2.registry.Hives))
	}

	// Bad JSON -> logged, registry left empty.
	os.WriteFile(registryPath, []byte(`{not json`), 0o644)
	s3 := &HubServer{logger: slog.Default()}
	s3.loadRegistry()
	if len(s3.registry.Hives) != 0 {
		t.Errorf("bad JSON should leave registry empty, got %d", len(s3.registry.Hives))
	}
}

func TestHandleApproveProvision(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := &HubServer{logger: slog.Default(), saveCh: make(chan struct{}, 1), clusters: map[string]ClusterConfig{"hive-oke": *dynamicCluster()}}

	// No pending request -> 404.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/approve-prov", "", hubAdminUsername), "username", "bob")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-request approve status = %d, want 404", rec.Code)
	}

	// Pending request but no available placeholder -> 409.
	saveProvisionRequest(&ProvisionRequest{
		Username: "bob", Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2,
		AuthMethod: "public", RequestedAt: time.Now().UTC().Format(time.RFC3339), Status: provisionStatusPending,
	})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/approve-prov", "", hubAdminUsername), "username", "bob")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("no-placeholder approve status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}

	// With an available placeholder -> success.
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/approve-prov", "", hubAdminUsername), "username", "bob")
	s.handleApproveProvision(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve success status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleToggleAutoUpgradeHeartbeatOnly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")

	// No SaaS record; only a registry entry (heartbeat-connected hive behind latest).
	s := &HubServer{logger: slog.Default(), heartbeatUpgrade: make(map[string]string)}
	s.registry.Hives = []RegistryEntry{{ID: "hb1", Owner: "alice", GitBranch: "v2", GitHash: "behind0"}}
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "latest9"}
	latestSHAMu.Unlock()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPut, "/au", `{"auto_upgrade":true}`, "alice"), "id", "hb1")
	s.handleToggleAutoUpgrade(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat-only toggle status = %d body=%s", rec.Code, rec.Body.String())
	}
	// A minimal SaaS record should now exist with auto_upgrade set.
	if got := loadSaaSHive("hb1"); got == nil || !got.AutoUpgrade {
		t.Errorf("expected minimal SaaS record with auto_upgrade, got %+v", got)
	}
}

func TestHandleToggleAutoUpgradeUnknownHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	s := &HubServer{logger: slog.Default(), heartbeatUpgrade: make(map[string]string)}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPut, "/au", `{"auto_upgrade":true}`, "alice"), "id", "nope")
	s.handleToggleAutoUpgrade(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown-hive toggle status = %d, want 404", rec.Code)
	}
}

func TestHandleMyHives(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Owner with a SaaS hive.
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", Org: "acme", Status: "running"})

	s := &HubServer{logger: slog.Default()}
	// A registry entry for the same hive to exercise the merge path.
	s.registry.Hives = []RegistryEntry{{ID: "h1", Owner: "alice", Org: "acme", GitHash: "abc", Online: true}}

	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "alice")
	s.handleMyHives(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("my-hives status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal my-hives: %v", err)
	}
}

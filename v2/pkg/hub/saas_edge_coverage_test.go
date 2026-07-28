package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// saas.go — edge/error branches across hub handlers
// ============================================================

// noClusterHub is a hub whose clusters map is empty, so clusterForHive returns
// nil and the "no cluster config" error branches run.
func noClusterHub() *HubServer {
	return &HubServer{
		logger:             slog.Default(),
		saveCh:             make(chan struct{}, 1),
		heartbeatUpgrade:   make(map[string]string),
		heartbeatSwitchTag: make(map[string]string),
		clusters:           map[string]ClusterConfig{},
	}
}

func TestHandleUpgradeHiveNoCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "gone"})
	s := noClusterHub()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/up", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("no-cluster upgrade status = %d, want 500", rec.Code)
	}
}

func TestHandleUpgradeHiveRegistryUpdate(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}}}
	// A matching registry entry so the post-upgrade registry-update loop runs.
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "latest7"}
	latestSHAMu.Unlock()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/up", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upgrade status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	up := s.registry.Hives[0].Upgrading
	s.mu.RUnlock()
	if !up {
		t.Error("expected registry entry marked Upgrading after upgrade")
	}
}

func TestHandleDeleteHiveNoCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "gone"})
	s := noClusterHub()

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodDelete, "/del", "", "alice"), "id", "h1")
	s.handleDeleteHive(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("no-cluster delete status = %d, want 500", rec.Code)
	}
}

func TestHandleHubAutoUpgradeSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// latest SHA differs from hubGitHash -> initial trigger fires (kubectl OK).
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newhubsha"}
	latestSHAMu.Unlock()

	s := &HubServer{
		logger:     slog.Default(),
		hubGitHash: "oldhubsha",
		clusters:   map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}},
	}
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/hub-au", `{"auto_upgrade":true}`, "admin")
	s.handleHubAutoUpgrade(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hub auto-upgrade status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Bad body -> 400.
	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPost, "/hub-au", `{bad`, "admin")
	s.handleHubAutoUpgrade(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad-body hub auto-upgrade status = %d, want 400", rec.Code)
	}
}

func TestHandleHubSelfUpgrade_Edge(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// A latest v2 SHA must be resolved for the handler to have a target.
	setV2Latest(t, "target1")
	// Stub the GHCR hub-image check so tests don't hit the network; assume the
	// hub image exists for the success path.
	oldImgCheck := hubImageExists
	hubImageExists = func(sha string, _ *slog.Logger) bool { return true }
	t.Cleanup(func() { hubImageExists = oldImgCheck })

	// Failure path: fail-fast fake kubectl (exit 1) -> 500.
	s := &HubServer{logger: slog.Default(), hubGitBranch: "v2", clusters: map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID, InCluster: true}}}
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/hub-self", "", "admin")
	s.handleHubSelfUpgrade(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("hub self-upgrade (kubectl fail) status = %d, want 500", rec.Code)
	}

	// Success path with scripted kubectl.
	installScriptedKubectl(t)
	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPost, "/hub-self", "", "admin")
	s.handleHubSelfUpgrade(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("hub self-upgrade (kubectl ok) status = %d, want 200", rec.Code)
	}

	// Missing hub image -> handler fails even with a healthy kubectl.
	hubImageExists = func(sha string, _ *slog.Logger) bool { return false }
	rec = httptest.NewRecorder()
	req = reqWithUser(http.MethodPost, "/hub-self", "", "admin")
	s.handleHubSelfUpgrade(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("hub self-upgrade (missing hub image) status = %d, want 500", rec.Code)
	}
}

func TestHandleClusterHealthBuildError(t *testing.T) {
	// buildClusterHealth returns an error only if it cannot proceed at all;
	// exercise the handler with no clusters (empty per-cluster result, still 200).
	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	s := &HubServer{
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ch", nil)
	s.handleClusterHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("empty-clusters health status = %d, want 200", rec.Code)
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()
}

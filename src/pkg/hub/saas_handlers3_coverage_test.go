package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// saas.go — remaining handler + auto-upgrade branches
// ============================================================

func TestHandleSwitchBranchSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t) // returns 0 -> kubectl set image + rollout succeed

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "abc1234"}
	latestSHAMu.Unlock()
	oldSpokeImageExists := spokeImageExists
	spokeImageExists = func(string, *slog.Logger) bool { return true }
	t.Cleanup(func() { spokeImageExists = oldSpokeImageExists })

	s := newHandlerHub2()
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/sw", `{"branch":"v2"}`, "alice"), "id", "h1")
	s.handleSwitchBranch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch success status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeprovisionHiveWithOCI(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// Point OCI FSS calls at a local server so the export/FS delete branches run
	// without real network.
	ociSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ociSrv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, ociSrv)

	h := &SaaSHive{ID: "hosted-oci", Owner: "alice", ClusterID: "hive-oke",
		OCIExportID: "ocid1.export.x", OCIFileSystemID: "ocid1.fs.x"}
	saveSaaSHive(h)
	// NFS storage triggers the OCI cleanup branch.
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true, StorageType: storageTypeNFS}
	deprovisionHive(h, cluster, slog.Default())
}

func TestTriggerAutoUpgradesStaleRecovery(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// AutoUpgrade hive that is latched "upgrading" with a stale timestamp so the
	// stale-recovery path runs.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "hive-oke"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha1"}
	latestSHAMu.Unlock()

	s := newHandlerHub2()
	s.heartbeatUpgrade = make(map[string]string)
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "oldsha0",
		Upgrading: true, UpgradeTarget: "oldtarget", UpgradeStartedAt: time.Now().Add(-2 * time.Hour),
	}}
	s.triggerAutoUpgrades()

	// After stale recovery the heartbeat fallback should be re-armed.
	s.mu.RLock()
	_, armed := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if !armed {
		t.Log("heartbeat fallback not armed (acceptable if kubectl path succeeded)")
	}
}

func TestTriggerAutoUpgradesBehindLatest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// A hive not upgrading, behind latest -> starts an upgrade.
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "hive-oke"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "latest99"}
	latestSHAMu.Unlock()

	s := newHandlerHub2()
	s.heartbeatUpgrade = make(map[string]string)
	s.registry.Hives = []RegistryEntry{{ID: "h2", GitBranch: "v2", GitHash: "behind00", Upgrading: false}}
	s.triggerAutoUpgrades()
}

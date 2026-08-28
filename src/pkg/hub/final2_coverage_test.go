package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas.go — triggerAutoUpgrades additional branches (remote cluster, no cluster)
// ============================================================

func TestTriggerAutoUpgradesRemoteStale(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// A remote (not in-cluster) cluster so the kubectl-restart branch runs during
	// stale recovery.
	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx", Domain: "r.example.com"}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha7"}
	latestSHAMu.Unlock()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "old", Upgrading: true,
		UpgradeTarget: "oldtarget", UpgradeStartedAt: time.Now().Add(-2 * time.Hour),
	}}
	s.triggerAutoUpgrades()
}

func TestTriggerAutoUpgradesRemoteNotStale(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})

	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "remote"})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{"remote": remote}}
	// Upgrading, recent timestamp -> not stale -> re-arm heartbeat target.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2", GitHash: "old", Upgrading: true,
		UpgradeTarget: "keepTarget", UpgradeStartedAt: time.Now(),
		// Heartbeating, so the upgrade is collectible and delivery is re-armed
		// (uncollectible hives are covered in rearm_collectible_test.go).
		LastHeartbeat: rfc3339At(time.Now().Add(-time.Minute)),
	}}
	s.triggerAutoUpgrades()
	s.mu.RLock()
	got := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if got != "keepTarget" {
		t.Errorf("expected heartbeat target re-armed, got %q", got)
	}
}

func TestTriggerAutoUpgradesNoClusterSkip(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "gone"})
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha"}
	latestSHAMu.Unlock()

	// Not upgrading, behind latest, but no cluster config -> skip branch.
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string), clusters: map[string]ClusterConfig{}}
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2", GitHash: "old"}}
	s.triggerAutoUpgrades()
}

// ============================================================
// saas.go — handleOpenHive no-secret plain redirect
// ============================================================

func TestHandleOpenHiveNoSecret(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)

	// No hub secret -> cannot mint SSO token, falls back to a plain dashboard
	// redirect at the resolved base URL.
	s := &HubServer{logger: slog.Default(), hubSecret: ""}
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodGet, "/open", "", hubAdminUsername), "id", "hosted-x")
	s.handleOpenHive(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("open (no secret) status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "/sso?token=") {
		t.Errorf("expected plain redirect without SSO token, got %q", loc)
	}
}

// ============================================================
// openrouter.go — handleHubOpenRouterStart success (state + authorize URL)
// ============================================================

func TestHandleHubOpenRouterStartOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Owner (not admin) funds their own hive.
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice"})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, pendingGateways: make(map[string]*HeartbeatGatewayConfig)}
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodGet, "/api/openrouter/connect/start?hive_id=h1", "", "alice")
	s.handleHubOpenRouterStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner start status = %d body=%s", rec.Code, rec.Body.String())
	}
}

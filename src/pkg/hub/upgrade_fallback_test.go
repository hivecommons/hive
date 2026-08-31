package hub

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedLatestSHA installs a fake image-verified head SHA for a branch and
// restores the previous cache on cleanup, so tests don't leak into each other.
func seedLatestSHA(t *testing.T, branch, sha string) {
	t.Helper()
	latestSHAMu.Lock()
	prev, had := latestSHAByBranch[branch]
	latestSHAByBranch[branch] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		if had {
			latestSHAByBranch[branch] = prev
		} else {
			delete(latestSHAByBranch, branch)
		}
		latestSHAMu.Unlock()
	})
}

// An upgrade against a cluster the hub cannot reach must not fail — it must
// arm the heartbeat fallback, which is the only delivery path that works for
// firewalled clusters (the spoke pulls; the hub answers). This is the exact
// production case of the vllm-d cluster: every manual upgrade 504ed and armed
// nothing, so hives there could never be upgraded from the hub.
func TestHandleUpgradeHiveUnreachableClusterArmsHeartbeatFallback(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "vllm-d"})
	s := &HubServer{
		hubSecret: testHubSecret,
		logger:    slog.Default(),
		clusters:  map[string]ClusterConfig{"vllm-d": {ID: "vllm-d"}},
		// The unreachable cache is pre-populated, exactly as it is in
		// production after the first failed kubectl: rolloutRestartHive skips
		// kubectl entirely and reports non-delivery.
		clusterUnreachableUntil: map[string]time.Time{"vllm-d": time.Now().Add(time.Hour)},
	}
	// Heartbeating: the manual upgrade path refuses a hive that cannot COLLECT
	// the instruction (pull-only delivery, see pullonly_upgrade.go). That gate is
	// independent of what this test is about.
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}
	seedLatestSHA(t, "v2", "abc1234")

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)

	if rec.Code != 200 {
		t.Fatalf("unreachable-cluster upgrade status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mode":"heartbeat"`) {
		t.Errorf("response should name the heartbeat mode, got %s", rec.Body.String())
	}
	s.mu.Lock()
	armed := s.heartbeatUpgrade["h1"]
	var latched bool
	var target string
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == "h1" {
			latched = s.registry.Hives[i].Upgrading
			target = s.registry.Hives[i].UpgradeTarget
		}
	}
	s.mu.Unlock()
	if armed != "abc1234" {
		t.Errorf("heartbeat fallback armed with %q, want abc1234", armed)
	}
	if !latched || target != "abc1234" {
		t.Errorf("registry not latched: upgrading=%v target=%q, want true/abc1234", latched, target)
	}
}

// When kubectl cannot deliver AND no image-verified build target exists for
// the branch, there is genuinely nothing a heartbeat could carry — that is the
// one remaining hard-failure case, and it must say so rather than pretend.
func TestHandleUpgradeHiveUnreachableClusterWithoutTargetFails(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "alice", ClusterID: "vllm-d"})
	s := &HubServer{
		hubSecret:               testHubSecret,
		logger:                  slog.Default(),
		clusters:                map[string]ClusterConfig{"vllm-d": {ID: "vllm-d"}},
		clusterUnreachableUntil: map[string]time.Time{"vllm-d": time.Now().Add(time.Hour)},
	}
	// The registry names a branch with NO cached SHA: no target can exist.
	// Heartbeating: the manual upgrade path refuses a hive that cannot COLLECT
	// the instruction (pull-only delivery, see pullonly_upgrade.go). That gate is
	// independent of what this test is about.
	s.registry.Hives = []RegistryEntry{{ID: "h2", GitBranch: "branch-with-no-builds", LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "h2")
	s.handleUpgradeHive(rec, req)

	if rec.Code != 502 {
		t.Fatalf("no-target upgrade status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	s.mu.Lock()
	_, armed := s.heartbeatUpgrade["h2"]
	s.mu.Unlock()
	if armed {
		t.Error("heartbeat fallback must not be armed when there is no target SHA")
	}
}

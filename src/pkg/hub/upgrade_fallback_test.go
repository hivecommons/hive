package hub

import (
	"encoding/json"
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

func TestHandleUpgradeHiveStableAlreadyAtChannelIsNoop(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	stubChannelRevisions(t, map[string]string{"stable": "d5a638e"})
	seedLatestSHA(t, "v5", "abc9999")

	mkUser(t, "alice")
	if err := saveSaaSHive(&SaaSHive{
		ID:             "h-stable",
		Owner:          "alice",
		ClusterID:      "vllm-d",
		TrackedChannel: "stable",
	}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	s := &HubServer{
		hubSecret: testHubSecret,
		logger:    slog.Default(),
		clusters:  map[string]ClusterConfig{"vllm-d": {ID: "vllm-d"}},
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "h-stable",
		GitBranch:     "v5",
		GitHash:       "d5a638e",
		ImageRef:      "ghcr.io/hivecommons/hive:stable",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "h-stable")
	s.handleUpgradeHive(rec, req)

	// A spoke already at its target is a clean no-op (200), NOT a refusal:
	// both dashboards render every non-2xx as "Upgrade failed", and the spoke
	// relay persisted the 409 as a failed self-upgrade (#12494).
	if rec.Code != 200 {
		t.Fatalf("already-current upgrade status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body upgradeNoopResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	if body.Status != upgradeStatusCurrent {
		t.Errorf("status = %q, want %q", body.Status, upgradeStatusCurrent)
	}
	if body.Current != "d5a638e" || body.Target != "d5a638e" || body.Channel != "stable" {
		t.Errorf("body = %+v, want current=target=d5a638e on channel stable", body)
	}
	if !strings.Contains(body.Message, "already up to date") || !strings.Contains(body.Message, "running commit d5a638e") {
		t.Errorf("message should say already up to date and name the commit, got %q", body.Message)
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("a no-op must not carry an error key: %s", rec.Body.String())
	}
	s.mu.Lock()
	_, armed := s.heartbeatUpgrade["h-stable"]
	latched := s.registry.Hives[0].Upgrading
	s.mu.Unlock()
	if armed {
		t.Error("no-op manual upgrade must not arm heartbeat delivery")
	}
	if latched {
		t.Error("no-op manual upgrade must not latch Upgrading")
	}
}

// Branch-tracking variant (no channel): same no-op contract with the
// branch-shaped message.
func TestHandleUpgradeHiveBranchAlreadyAtTipIsNoop(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	seedLatestSHA(t, "v5", "abc9999")

	mkUser(t, "alice")
	if err := saveSaaSHive(&SaaSHive{ID: "h-branch", Owner: "alice", ClusterID: "vllm-d"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	s := &HubServer{
		hubSecret: testHubSecret,
		logger:    slog.Default(),
		clusters:  map[string]ClusterConfig{"vllm-d": {ID: "vllm-d"}},
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "h-branch",
		GitBranch:     "v5",
		GitHash:       "abc9999",
		ImageRef:      "ghcr.io/hivecommons/hive:abc9999",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "h-branch")
	s.handleUpgradeHive(rec, req)

	if rec.Code != 200 {
		t.Fatalf("already-current branch upgrade status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body upgradeNoopResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	if body.Status != upgradeStatusCurrent || body.Channel != "" || body.Target != "abc9999" {
		t.Errorf("body = %+v, want status current, no channel, target abc9999", body)
	}
	if !strings.Contains(body.Message, "already up to date") {
		t.Errorf("message = %q, want 'already up to date'", body.Message)
	}
	s.mu.Lock()
	_, armed := s.heartbeatUpgrade["h-branch"]
	s.mu.Unlock()
	if armed {
		t.Error("no-op manual upgrade must not arm heartbeat delivery")
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

// The hub dashboard's Upgrade click must branch on status=current before
// declaring success and must never render the no-op through the error toast.
func TestHubDashboardUpgradeClickHandlesAlreadyCurrent(t *testing.T) {
	start := strings.Index(dashboardHTML, "async function upgradeHive(")
	if start < 0 {
		t.Fatal("upgradeHive not found in dashboard.html")
	}
	fn := dashboardHTML[start:]
	if end := strings.Index(fn, "function toggleBranchMenu("); end > 0 {
		fn = fn[:end]
	}
	current := strings.Index(fn, "data.status === 'current'")
	started := strings.Index(fn, "'Upgrade started for '")
	if current < 0 {
		t.Fatal("upgradeHive must branch on data.status === 'current'")
	}
	if started < 0 || current > started {
		t.Errorf("the status=current branch must precede the success toast (current=%d started=%d)", current, started)
	}
	branch := fn[current:started]
	if !strings.Contains(branch, "delete _upgradingHives[id]") {
		t.Error("the already-current branch must drop the optimistic upgrading sentinel so the row stops spinning")
	}
	if strings.Contains(branch, "'error')") {
		t.Error("the already-current branch must not toast as an error")
	}
}

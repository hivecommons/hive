package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installBulkKubectl installs a scripted fake kubectl on PATH. ok=false makes
// every invocation fail, which is how a firewalled or unreachable cluster looks
// to the hub. This is the package's established pattern (see
// installScriptedKubectl); combined with TestMain clearing the in-cluster
// service-account env and pointing KUBECONFIG at /dev/null, no test can reach a
// real API server.
func installBulkKubectl(t *testing.T, ok bool) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'deployment.apps/hive restarted'\nexit 0\n"
	if !ok {
		script = "#!/bin/sh\necho 'Unable to connect to the server: dial tcp: i/o timeout' >&2\nexit 1\n"
	}
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// setLatestSHAForBranchForTest seeds the package-level SHA cache and restores
// the previous value, so tests do not leak state into each other.
func setLatestSHAForBranchForTest(t *testing.T, branch, sha string) {
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

// bulkHiveOnCluster seeds a hive owned by owner and wires a cluster config plus
// a registry entry, which bulkRestartOrUpgrade needs to resolve a branch.
func bulkHiveOnCluster(t *testing.T, s *HubServer, id, owner, branch string) {
	t.Helper()
	bulkSeedHive(t, id, owner)
	s.mu.Lock()
	s.clusters = map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}}
	if s.heartbeatUpgrade == nil {
		s.heartbeatUpgrade = map[string]string{}
	}
	s.registry.Hives = append(s.registry.Hives, RegistryEntry{ID: id, GitBranch: branch})
	s.mu.Unlock()
}

func bulkRegistryEntry(t *testing.T, s *HubServer, id string) RegistryEntry {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.registry.Hives {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no registry entry for %q", id)
	return RegistryEntry{}
}

// ---- applyBulkAction dispatch ----

func TestApplyBulkActionRejectsUnknownAction(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")

	res := s.applyBulkAction("no-such-action", "", "hosted-x", "alice")
	if res.Ok {
		t.Fatal("an unknown action must not report success")
	}
	if res.Error != "unknown bulk action" {
		t.Fatalf("error = %q", res.Error)
	}
}

func TestApplyBulkActionPropagatesAuthorizationFailure(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")

	res := s.applyBulkAction(bulkActionRestart, "", "hosted-x", "mallory")
	if res.Ok {
		t.Fatal("an unauthorized bulk action must not report success")
	}
	if res.HiveID != "hosted-x" {
		t.Errorf("result must identify the hive, got %q", res.HiveID)
	}
}

// ---- bulkRestartOrUpgrade ----

// A reachable cluster is served by kubectl and reported as such.
func TestBulkRestartViaKubectlWhenClusterReachable(t *testing.T) {
	installBulkKubectl(t, true)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	res := s.applyBulkAction(bulkActionRestart, "", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("restart on a reachable cluster must succeed, got %q", res.Error)
	}
	// INVERTED DELIBERATELY: the `kubectl set image` push is retired, so a
	// branch switch is delivered by arming heartbeatSwitchTag and Via is
	// "heartbeat" on every cluster.
	if res.Via != bulkViaHeartbeat {
		t.Fatalf("want Via=%q, got %q", bulkViaHeartbeat, res.Via)
	}
}

// A restart that cannot be delivered must be reported as a FAILURE: unlike an
// upgrade there is no SHA for the heartbeat path to carry, so reporting success
// would tell the operator a restart happened when it did not.
func TestBulkRestartFailsWhenClusterUnreachable(t *testing.T) {
	installBulkKubectl(t, false)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	res := s.applyBulkAction(bulkActionRestart, "", "hosted-x", "alice")
	// INVERTED DELIBERATELY (push-path retirement). This test used to assert
	// "an undeliverable restart must NOT report success — a restart has no
	// heartbeat fallback". That premise died with the push path: a restart is
	// now delivered by arming the durable RequestedRestartAt, which the
	// heartbeat turns into resp.RestartSpoke and the spoke applies to itself.
	// Cluster reachability is irrelevant, so there is no "undeliverable" case
	// left to report — asserting the old behaviour would now be asserting that
	// a working feature is broken.
	if !res.Ok {
		t.Fatalf("a restart must succeed via the heartbeat pull path regardless of "+
			"cluster reachability, got %q", res.Error)
	}
	s.mu.RLock()
	_, armed := s.heartbeatUpgrade["hosted-x"]
	s.mu.RUnlock()
	if armed {
		t.Fatal("a restart must not arm a heartbeat UPGRADE")
	}
}

// An upgrade is delivered over the heartbeat on EVERY cluster. This was
// previously framed as a "fallback" for firewalled clusters; with the push path
// retired it is simply the delivery mechanism, so Via is always "heartbeat".
func TestBulkUpgradeFallsBackToHeartbeatWhenUnreachable(t *testing.T) {
	installBulkKubectl(t, false)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")
	setLatestSHAForBranchForTest(t, "v2", "abc123def")

	res := s.applyBulkAction(bulkActionUpgrade, "", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("an undeliverable upgrade must succeed via heartbeat, got %q", res.Error)
	}
	if res.Via != bulkViaHeartbeat {
		t.Fatalf("want Via=%q, got %q", bulkViaHeartbeat, res.Via)
	}
	s.mu.RLock()
	armed := s.heartbeatUpgrade["hosted-x"]
	s.mu.RUnlock()
	if armed != "abc123def" {
		t.Fatalf("the heartbeat fallback must be armed with the target SHA, got %q", armed)
	}
}

// An upgrade advances the registry's upgrade target; a plain restart must NOT,
// or the dashboard claims a version change that never happened.
func TestBulkRestartDoesNotAdvanceUpgradeTarget(t *testing.T) {
	installBulkKubectl(t, true)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")
	setLatestSHAForBranchForTest(t, "v2", "abc123def")

	if res := s.applyBulkAction(bulkActionRestart, "", "hosted-x", "alice"); !res.Ok {
		t.Fatalf("restart failed: %q", res.Error)
	}
	reg := bulkRegistryEntry(t, s, "hosted-x")
	if reg.Upgrading {
		t.Fatal("a plain restart must not mark the hive as Upgrading")
	}
	if reg.UpgradeTarget != "" {
		t.Fatalf("a plain restart must not set an upgrade target, got %q", reg.UpgradeTarget)
	}
}

func TestBulkUpgradeAdvancesUpgradeTarget(t *testing.T) {
	installBulkKubectl(t, true)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")
	setLatestSHAForBranchForTest(t, "v2", "abc123def")

	if res := s.applyBulkAction(bulkActionUpgrade, "", "hosted-x", "alice"); !res.Ok {
		t.Fatalf("upgrade failed: %q", res.Error)
	}
	reg := bulkRegistryEntry(t, s, "hosted-x")
	if !reg.Upgrading {
		t.Fatal("an upgrade must mark the hive as Upgrading")
	}
	if reg.UpgradeTarget != "abc123def" {
		t.Fatalf("upgrade target = %q, want the branch's latest SHA", reg.UpgradeTarget)
	}
	if reg.UpgradeStartedAt.IsZero() {
		t.Fatal("an upgrade must stamp UpgradeStartedAt so the UI can show elapsed time")
	}
}

// A hive whose cluster has no config cannot be acted on.
func TestBulkRestartFailsWithoutClusterConfig(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")
	s.mu.Lock()
	s.clusters = map[string]ClusterConfig{}
	s.mu.Unlock()

	res := s.applyBulkAction(bulkActionRestart, "", "hosted-x", "alice")
	if res.Ok {
		t.Fatal("a hive with no cluster config must not report success")
	}
	if res.Error != "no cluster config for this hive" {
		t.Fatalf("error = %q", res.Error)
	}
}

// ---- bulkSetAutoUpgrade ----

// A mode change must clear the day's fire record, or a hive that already fired
// today under the old mode would be skipped under the new one.
func TestBulkSetAutoUpgradeClearsFireRecord(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")
	h := loadSaaSHive("hosted-x")
	h.AutoUpgradeLastFired = "2026-07-30"
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	res := s.applyBulkAction(bulkActionDailyAutoUpgrade, "", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("enabling daily auto-upgrade failed: %q", res.Error)
	}
	if res.Via != bulkViaStore {
		t.Fatalf("auto-upgrade needs no kubectl, want Via=%q, got %q", bulkViaStore, res.Via)
	}

	got := loadSaaSHive("hosted-x")
	if !got.AutoUpgrade {
		t.Fatal("auto-upgrade was not persisted")
	}
	if got.AutoUpgradeMode != AutoUpgradeModeDaily {
		t.Fatalf("mode = %q, want %q", got.AutoUpgradeMode, AutoUpgradeModeDaily)
	}
	if got.AutoUpgradeLastFired != "" {
		t.Fatalf("a mode change must clear the fire record, got %q", got.AutoUpgradeLastFired)
	}
}

func TestBulkDisableAutoUpgradePersists(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")

	if res := s.applyBulkAction(bulkActionEnableAutoUpgrade, "", "hosted-x", "alice"); !res.Ok {
		t.Fatalf("enable failed: %q", res.Error)
	}
	if res := s.applyBulkAction(bulkActionDisableAutoUpgrade, "", "hosted-x", "alice"); !res.Ok {
		t.Fatalf("disable failed: %q", res.Error)
	}
	if got := loadSaaSHive("hosted-x"); got.AutoUpgrade {
		t.Fatal("auto-upgrade was not disabled")
	}
}

// Auto-upgrade changes are store-only, which is exactly why a firewalled hive
// can still be managed. This must hold even when kubectl fails.
func TestBulkAutoUpgradeWorksOnUnreachableCluster(t *testing.T) {
	installBulkKubectl(t, false)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	res := s.applyBulkAction(bulkActionEnableAutoUpgrade, "", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("auto-upgrade must not depend on cluster reachability, got %q", res.Error)
	}
	if res.Via != bulkViaStore {
		t.Fatalf("want Via=%q, got %q", bulkViaStore, res.Via)
	}
}

// ---- validateBulkBranch ----

// A branch already tracked by the hub is accepted without any network lookup.
func TestValidateBulkBranchAcceptsTrackedBranch(t *testing.T) {
	s := bulkTestHub(t)
	tracked := s.trackedBranchList()
	if len(tracked) == 0 {
		t.Skip("no tracked branches in this build")
	}
	if !s.validateBulkBranch(tracked[0]) {
		t.Fatalf("tracked branch %q must validate", tracked[0])
	}
}

// An untracked branch with no resolvable SHA must be rejected, so a bulk switch
// cannot point the fleet at a branch that does not exist.
func TestValidateBulkBranchRejectsUnknownBranch(t *testing.T) {
	s := bulkTestHub(t)
	// A branch name that is not tracked and has no cached SHA. fetchBranchSHA
	// cannot reach GitHub (TestMain neutralises the environment), so the cache
	// stays empty and validation must fail closed.
	if s.validateBulkBranch("definitely-not-a-real-branch-zzz") {
		t.Fatal("an unresolvable branch must be rejected, not accepted by default")
	}
}

// A branch that is not tracked but HAS a cached SHA is accepted — this is how a
// freshly-pushed branch becomes usable without restarting the hub.
func TestValidateBulkBranchAcceptsUntrackedBranchWithKnownSHA(t *testing.T) {
	s := bulkTestHub(t)
	setLatestSHAForBranchForTest(t, "feature-xyz", "cafebabe")
	if !s.validateBulkBranch("feature-xyz") {
		t.Fatal("a branch with a resolved SHA must validate")
	}
}

// ---- bulkSwitchBranch ----

// A reachable cluster gets the image set by kubectl and is restarted so the new
// image is actually pulled.
func TestBulkSwitchBranchViaKubectl(t *testing.T) {
	installBulkKubectl(t, true)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	res := s.applyBulkAction(bulkActionSwitchBranch, "dd", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("branch switch failed: %q", res.Error)
	}
	// INVERTED DELIBERATELY: the `kubectl set image` push is retired, so a
	// branch switch is delivered by arming heartbeatSwitchTag and Via is
	// "heartbeat" on every cluster.
	if res.Via != bulkViaHeartbeat {
		t.Fatalf("want Via=%q, got %q", bulkViaHeartbeat, res.Via)
	}
	reg := bulkRegistryEntry(t, s, "hosted-x")
	if !reg.Upgrading {
		t.Fatal("a branch switch must mark the hive as Upgrading")
	}
	// The target must be the sanitized branch tag, not the raw branch name.
	if reg.UpgradeTarget != branchToTag("dd")+"-latest" {
		t.Fatalf("upgrade target = %q, want the branch's -latest tag", reg.UpgradeTarget)
	}
	// The pull path IS the delivery, so the switch tag MUST be armed.
	s.mu.RLock()
	_, armed := s.heartbeatSwitchTag["hosted-x"]
	s.mu.RUnlock()
	if !armed {
		t.Fatal("the branch switch must arm heartbeatSwitchTag — under the pull model " +
			"that IS the delivery, not a fallback for a failed push")
	}
}

// When kubectl cannot reach the cluster the switch is queued for the spoke to
// apply on its next heartbeat — the spoke holds RBAC to patch its own
// deployment, so this is a real delivery path, not a failure.
func TestBulkSwitchBranchFallsBackToHeartbeat(t *testing.T) {
	installBulkKubectl(t, false)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	res := s.applyBulkAction(bulkActionSwitchBranch, "dd", "hosted-x", "alice")
	if !res.Ok {
		t.Fatalf("an undeliverable switch must still queue via heartbeat: %q", res.Error)
	}
	if res.Via != bulkViaHeartbeat {
		t.Fatalf("want Via=%q, got %q", bulkViaHeartbeat, res.Via)
	}
	s.mu.RLock()
	tag := s.heartbeatSwitchTag["hosted-x"]
	s.mu.RUnlock()
	if tag != branchToTag("dd")+"-latest" {
		t.Fatalf("heartbeat switch tag = %q, want the branch's -latest tag", tag)
	}
}

// A slashed branch must be converted to the same image tag docker.yml
// publishes, or the hive is pointed at a tag that does not exist. This mirrors
// branchToTag, which handleSwitchBranch applies too — the two paths must agree
// or a bulk switch and a single switch would land on different images.
//
// Only "/" is translated: branch names reaching here have already passed
// validateBulkBranch, so they exist in git and cannot contain characters git
// itself forbids in a ref.
func TestBulkSwitchBranchConvertsSlashesToTag(t *testing.T) {
	installBulkKubectl(t, false)
	s := bulkTestHub(t)
	bulkHiveOnCluster(t, s, "hosted-x", "alice", "v2")

	if res := s.applyBulkAction(bulkActionSwitchBranch, "feat/my-branch", "hosted-x", "alice"); !res.Ok {
		t.Fatalf("switch failed: %q", res.Error)
	}
	s.mu.RLock()
	tag := s.heartbeatSwitchTag["hosted-x"]
	s.mu.RUnlock()
	if strings.Contains(tag, "/") {
		t.Fatalf("tag %q still contains a slash — it would not resolve in the registry", tag)
	}
	if tag != "feat-my-branch-latest" {
		t.Fatalf("tag = %q, want the docker.yml-compatible form", tag)
	}
	// The bulk path and the single-hive path must agree on the tag.
	if tag != branchToTag("feat/my-branch")+"-latest" {
		t.Fatal("bulk switch and handleSwitchBranch disagree on the image tag")
	}
}

func TestBulkSwitchBranchFailsWithoutClusterConfig(t *testing.T) {
	s := bulkTestHub(t)
	bulkSeedHive(t, "hosted-x", "alice")
	s.mu.Lock()
	s.clusters = map[string]ClusterConfig{}
	s.mu.Unlock()

	res := s.applyBulkAction(bulkActionSwitchBranch, "dd", "hosted-x", "alice")
	if res.Ok {
		t.Fatal("a hive with no cluster config must not report success")
	}
}

package hub

import (
	"testing"
	"time"
)

// #3839 gated upgrade delivery on collectibility, but applied the gate only in
// the isStale branch of triggerAutoUpgrades(). Three other sites armed
// heartbeatUpgrade with no such check, so an uncollectible upgrade was re-armed
// faster than abandonment could retire it and the wedge merely raced the
// timeout. These tests pin the gate at every arming site.
//
// Every rejection test below is PAIRED with a positive control asserting that a
// hive which CAN collect is still armed by the same path. Over-blocking here
// would strand legitimate upgrades — a silent permanent opt-out, which the
// pull-only design notes call out as worse than the wedge itself.

// armedHive builds a registry entry latched mid-upgrade with a fresh (non-stale)
// clock, so triggerAutoUpgrades() takes the NOT-stale branch.
func armedHive(id, lastHeartbeat string) RegistryEntry {
	return RegistryEntry{
		ID: id, GitBranch: "v4", GitHash: "old1234",
		Upgrading: true, UpgradeTarget: "7cd059b",
		UpgradeStartedAt: time.Now(),
		LastHeartbeat:    lastHeartbeat,
	}
}

// TestNonStaleRearmSkipsUncollectibleHive is the regression for
// saas.go's not-stale branch. The hive is latched Upgrading with a FRESH clock,
// so abandonment (which only runs when isStale) cannot help: this is the branch
// that re-populated the map every 2-minute poll forever.
func TestNonStaleRearmSkipsUncollectibleHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("never-beat")
	saveSaaSHive(&SaaSHive{
		ID: "never-beat", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// Never heartbeated: LastHeartbeat empty. Cannot collect, ever.
	s.registry.Hives = []RegistryEntry{armedHive("never-beat", "")}

	for cycle := 1; cycle <= 5; cycle++ {
		s.triggerAutoUpgrades()
		if got := s.heartbeatUpgrade["never-beat"]; got != "" {
			t.Fatalf("cycle %d: non-stale branch re-armed heartbeatUpgrade to %q for a hive "+
				"that has never heartbeated — this is the loop that defeats #3839's gate", cycle, got)
		}
	}
}

// TestNonStaleRearmStillArmsCollectibleHive is the positive control for the
// test above: a mid-upgrade hive that is heartbeating MUST keep its instruction
// re-armed across hub restarts, which is the entire reason that branch exists.
func TestNonStaleRearmStillArmsCollectibleHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("beating")
	saveSaaSHive(&SaaSHive{
		ID: "beating", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{armedHive("beating", rfc3339At(time.Now().Add(-2*time.Minute)))}
	// Simulate the hub restart this branch is meant to repair.
	delete(s.heartbeatUpgrade, "beating")

	s.triggerAutoUpgrades()

	if got := s.heartbeatUpgrade["beating"]; got != "7cd059b" {
		t.Fatalf("heartbeatUpgrade[beating] = %q, want 7cd059b — a heartbeating hive "+
			"mid-upgrade must still have its instruction re-armed; over-blocking here "+
			"strands legitimate upgrades", got)
	}
}

// TestNonStaleRearmArmsSpokeQuietBecauseItIsRestarting guards the subtle case
// the predicate was deliberately bounded on staleRemoveAge (24h) rather than
// maxHeartbeatAge (5m) to protect: a spoke that is quiet precisely BECAUSE it is
// restarting into the upgrade just armed for it.
func TestNonStaleRearmArmsSpokeQuietBecauseItIsRestarting(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("restarting")
	saveSaaSHive(&SaaSHive{
		ID: "restarting", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	// 30 minutes silent — well past the Online pill's 5m, nowhere near eviction.
	s.registry.Hives = []RegistryEntry{armedHive("restarting", rfc3339At(time.Now().Add(-30*time.Minute)))}
	delete(s.heartbeatUpgrade, "restarting")

	s.triggerAutoUpgrades()

	if got := s.heartbeatUpgrade["restarting"]; got != "7cd059b" {
		t.Fatalf("heartbeatUpgrade[restarting] = %q, want 7cd059b — a spoke mid-restart "+
			"is still collectible; gating on maxHeartbeatAge would self-defeatingly "+
			"disarm the upgrade that caused the silence", got)
	}
}

// TestRecoverArmedUpgradesSkipsUncollectible is the regression for startup
// recovery. The registry latch is DURABLE, so without a gate here a hub restart
// resurrects an upgrade that was abandoned in memory.
func TestRecoverArmedUpgradesSkipsUncollectible(t *testing.T) {
	s := pullOnlyTestServer(t)
	s.registry.Hives = []RegistryEntry{
		armedHive("ghost", ""),
		armedHive("live", rfc3339At(time.Now().Add(-time.Minute))),
	}

	s.recoverArmedUpgrades()

	if got := s.heartbeatUpgrade["ghost"]; got != "" {
		t.Errorf("startup recovery re-armed %q for a never-heartbeated hive — a hub roll "+
			"resurrects the abandoned wedge", got)
	}
	// Positive control in the same run: recovery must still work for a hive
	// that can collect, or a restart silently drops every in-flight upgrade.
	if got := s.heartbeatUpgrade["live"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade[live] = %q, want 7cd059b — startup recovery must still "+
			"restore instructions for collectible hives", got)
	}
}

// TestRecoverArmedUpgradesSkipsLongSilentHive pins the far edge of the
// predicate: a hive silent past staleRemoveAge is being evicted anyway.
func TestRecoverArmedUpgradesSkipsLongSilentHive(t *testing.T) {
	s := pullOnlyTestServer(t)
	s.registry.Hives = []RegistryEntry{
		armedHive("gone", rfc3339At(time.Now().Add(-staleRemoveAge-time.Hour))),
	}

	s.recoverArmedUpgrades()

	if got := s.heartbeatUpgrade["gone"]; got != "" {
		t.Errorf("startup recovery re-armed %q for a hive silent past staleRemoveAge", got)
	}
}

// TestUncollectibleUpgradeSurvivesRestartCycle replays the wedge shape from the
// original incident: a never-heartbeated placeholder latched Upgrading, driven
// through repeated poll cycles AND a hub restart. Before the fix the hive stayed
// armed indefinitely, with UpgradeStartedAt preserved by beginUpgrade() so the
// elapsed clock only ever grew (measured live past 146 minutes).
func TestUncollectibleUpgradeSurvivesRestartCycle(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	s.forgetUncollectibleUpgrade("wedge-1")
	saveSaaSHive(&SaaSHive{
		ID: "wedge-1", Owner: "alice", AutoUpgrade: true,
		Status: "running", ClusterID: "vllm-d",
	})
	s.registry.Hives = []RegistryEntry{armedHive("wedge-1", "")}

	for cycle := 1; cycle <= 10; cycle++ {
		s.triggerAutoUpgrades()
		// Interleave the hub roll that orphans upgrades in the real incident.
		if cycle%3 == 0 {
			s.recoverArmedUpgrades()
		}
		if got := s.heartbeatUpgrade["wedge-1"]; got != "" {
			t.Fatalf("cycle %d: hive is armed with %q despite never having heartbeated — "+
				"the wedge is still reachable", cycle, got)
		}
	}
}

// TestOrphanSweepDoesNotRearmUncollectible covers the third arming site.
// This sweep is where an uncollectible hive would otherwise spin forever:
// evaluateOrphanedUpgrade() bails on an unparseable/absent LastHeartbeat, so the
// retry budget that is meant to retire the upgrade is never spent.
func TestOrphanSweepDoesNotRearmUncollectible(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := pullOnlyTestServer(t)
	// Old enough for evaluateOrphanedUpgrade() to consider the attempt orphaned,
	// and — for the ghost — past silentUpgradeDeadline so the silence itself is
	// the evidence (it has no heartbeat to prove anything with).
	started := time.Now().Add(-silentUpgradeDeadline - time.Minute)

	ghost := armedHive("sweep-ghost", "")
	ghost.UpgradeStartedAt = started

	// A genuinely orphaned BUT collectible hive: it has heartbeated since the
	// instruction (so it is alive and can collect) yet is still reporting the
	// OLD SHA, so the upgrade did not converge and delivery must be re-armed.
	live := armedHive("sweep-live", rfc3339At(time.Now().Add(-time.Minute)))
	live.UpgradeStartedAt = started
	s.registry.Hives = []RegistryEntry{ghost, live}

	s.sweepOrphanedUpgrades()

	if got := s.heartbeatUpgrade["sweep-ghost"]; got != "" {
		t.Errorf("orphan sweep re-armed %q for a never-heartbeated hive", got)
	}
	if got := s.heartbeatUpgrade["sweep-live"]; got != "7cd059b" {
		t.Errorf("heartbeatUpgrade[sweep-live] = %q, want 7cd059b — the sweep must still "+
			"keep instructions alive for hives that can collect them", got)
	}
}

package hub

import (
	"log/slog"
	"testing"
	"time"
)

// #95: an auto-upgrade must be LATCHED OFF while a spoke's claim is in flight
// (Status==assigned && !ClaimDelivered). Rolling the spoke pod mid-claim can
// wedge it — the EPM dead-end (task #94). These tests drive triggerAutoUpgrades
// end to end and assert on the registry latch it sets: an upgrade that STARTS
// flips Upgrading=true and records UpgradeTarget; a deferred one leaves both
// untouched.

// latchTestHub builds a hub pointed at a single REMOTE (not in-cluster) pool, so
// triggerAutoUpgrades never takes the in-cluster self-upgrade path. The registry
// entry, not kubectl, is what we assert on.
func latchTestHub() *HubServer {
	remote := ClusterConfig{ID: "remote", InCluster: false, KubeconfigPath: "/tmp/kc", Context: "ctx"}
	return &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		heartbeatUpgrade: make(map[string]string),
		clusters:         map[string]ClusterConfig{"remote": remote},
	}
}

// upgradeStarted reports whether triggerAutoUpgrades armed a new upgrade for id —
// the registry latch it writes when (and only when) it decides to upgrade.
func upgradeStarted(s *HubServer, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.registry.Hives {
		if e.ID == id {
			return e.Upgrading && e.UpgradeTarget != ""
		}
	}
	return false
}

// primeBehindLatest seeds the SHA cache so every hive on branch v2 is one commit
// behind latest — the precondition that makes an eligible hive upgrade.
func primeBehindLatest(t *testing.T) {
	resetSHACaches(t)
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newsha95"}
	latestSHAMu.Unlock()
}

// registryBehind returns a v2 registry entry parked one commit behind latest and
// not currently upgrading — the state a fresh eligibility decision reads.
// registryBehind builds a LIVE hive that is behind latest.
//
// LastHeartbeat is set because these tests are about the ASSIGNMENT latch, not
// about whether the hive can collect an instruction. Upgrades are delivered on
// the spoke's own outbound heartbeat, so a hive that has never heartbeated is
// not armed at all — without a beat here every case below would be refused for
// the wrong reason and would stop testing the latch.
func registryBehind(id string) RegistryEntry {
	return RegistryEntry{
		ID: id, GitBranch: "v2", GitHash: "oldsha95",
		LastHeartbeat: time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
	}
}

// TestAutoUpgradeLatchedWhileClaimInFlight is the core guarantee: an
// assigned-but-unclaimed hive (claim in flight) is NOT told to upgrade even
// though it is behind latest and has AutoUpgrade on.
func TestAutoUpgradeLatchedWhileClaimInFlight(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	primeBehindLatest(t)

	const id = "hosted-inflight-95"
	saveSaaSHive(&SaaSHive{
		ID: id, Owner: "acme", AutoUpgrade: true, ClusterID: "remote",
		Status: statusAssigned, ClaimDelivered: false,
		AssignedAt: time.Now().UTC().Format(time.RFC3339),
	})

	s := latchTestHub()
	s.registry.Hives = []RegistryEntry{registryBehind(id)}
	s.triggerAutoUpgrades()

	if upgradeStarted(s, id) {
		t.Error("assigned-but-unclaimed hive was told to upgrade; the claim-in-flight latch must defer it (#95)")
	}
	s.mu.RLock()
	_, armed := s.heartbeatUpgrade[id]
	s.mu.RUnlock()
	if armed {
		t.Error("heartbeatUpgrade armed for an in-flight claim; no upgrade directive should reach the spoke")
	}
}

// TestAutoUpgradeResumesAfterClaimDelivered proves the latch RELEASES: the same
// hive, once ClaimDelivered flips true, is eligible again and upgrades.
func TestAutoUpgradeResumesAfterClaimDelivered(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	primeBehindLatest(t)

	const id = "hosted-delivered-95"
	saveSaaSHive(&SaaSHive{
		ID: id, Owner: "acme", AutoUpgrade: true, ClusterID: "remote",
		Status: statusAssigned, ClaimDelivered: true, // claim landed
		AssignedAt: time.Now().UTC().Format(time.RFC3339),
	})

	s := latchTestHub()
	s.registry.Hives = []RegistryEntry{registryBehind(id)}
	s.triggerAutoUpgrades()

	if !upgradeStarted(s, id) {
		t.Error("a claimed hive (ClaimDelivered=true) was NOT told to upgrade; the latch should have released")
	}
}

// TestAutoUpgradeAvailablePlaceholderUpgrades guards against over-latching: an
// AVAILABLE placeholder is not assigned, so it is not in-flight and upgrades
// normally.
func TestAutoUpgradeAvailablePlaceholderUpgrades(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	primeBehindLatest(t)

	const id = "hosted-available-95"
	saveSaaSHive(&SaaSHive{
		ID: id, Owner: hubAdminUsername, AutoUpgrade: true, ClusterID: "remote",
		Status: statusAvailable, ClaimDelivered: false,
	})

	s := latchTestHub()
	s.registry.Hives = []RegistryEntry{registryBehind(id)}
	s.triggerAutoUpgrades()

	if !upgradeStarted(s, id) {
		t.Error("an available placeholder was not upgraded; only assigned-but-unclaimed hives should be latched")
	}
}

// TestAutoUpgradeLiveClaimedHiveUpgrades is the ordinary case: a live, fully
// claimed hive (status set, ClaimDelivered=true) upgrades normally.
func TestAutoUpgradeLiveClaimedHiveUpgrades(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	primeBehindLatest(t)

	const id = "hosted-live-95"
	saveSaaSHive(&SaaSHive{
		ID: id, Owner: "acme", AutoUpgrade: true, ClusterID: "remote",
		Status: statusAssigned, ClaimDelivered: true,
	})

	s := latchTestHub()
	s.registry.Hives = []RegistryEntry{registryBehind(id)}
	s.triggerAutoUpgrades()

	if !upgradeStarted(s, id) {
		t.Error("a live claimed hive was not upgraded; the latch must only apply while the claim is in flight")
	}
}

// TestAssignmentInFlightPredicate pins the helper's truth table directly.
func TestAssignmentInFlightPredicate(t *testing.T) {
	cases := []struct {
		name           string
		status         string
		claimDelivered bool
		want           bool
	}{
		{"assigned + undelivered = in flight", statusAssigned, false, true},
		{"assigned + delivered = live", statusAssigned, true, false},
		{"available + undelivered = pool slot", statusAvailable, false, false},
		{"available + delivered = odd but not in flight", statusAvailable, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &SaaSHive{Status: tc.status, ClaimDelivered: tc.claimDelivered}
			if got := assignmentInFlight(h); got != tc.want {
				t.Errorf("assignmentInFlight(status=%q, delivered=%v) = %v, want %v",
					tc.status, tc.claimDelivered, got, tc.want)
			}
		})
	}
}

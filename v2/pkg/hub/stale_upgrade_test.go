package hub

import (
	"log/slog"
	"testing"
	"time"
)

// fixedSweepNow is an arbitrary but stable clock for these tests.
var fixedSweepNow = time.Date(2026, 7, 31, 14, 40, 0, 0, time.UTC)

// TestEvaluateOrphanedUpgrade covers the decision that separates an upgrade
// whose attempt has vanished from one that may still be in flight.
func TestEvaluateOrphanedUpgrade(t *testing.T) {
	// The production incident this fix exists for: hive
	// hosted-available-oke-05-placeholder-6q84 sat Upgrading for 68 minutes
	// while heartbeating normally on the OLD SHA, because its pod died between
	// the instruction and any completion/failure report.
	started := fixedSweepNow.Add(-68 * time.Minute)

	cases := []struct {
		name     string
		entry    RegistryEntry
		orphaned bool
	}{
		{
			name: "orphaned: alive and heartbeating past the threshold on the old SHA",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned: true,
		},
		{
			name: "in flight: silent since the upgrade was instructed (pod restarting)",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(started.Add(-time.Minute)),
			},
			orphaned: false,
		},
		{
			name: "in flight: a slow but recent upgrade is never cleared on elapsed time alone",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: fixedSweepNow.Add(-staleUpgradeTimeout - time.Minute),
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-10 * time.Second)),
			},
			orphaned: false,
		},
		{
			name: "not orphaned: spoke already reports the target, heartbeat path clears it",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "fc32ae4",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned: false,
		},
		{
			name: "not orphaned: an explicitly reported failure keeps its known cause",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeFailed:    true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned: false,
		},
		{
			name: "not orphaned: never heartbeated, so we cannot prove the attempt is gone",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
			},
			orphaned: false,
		},
		{
			name: "not orphaned: no start timestamp means no elapsed time to judge",
			entry: RegistryEntry{
				Upgrading:     true,
				GitHash:       "c11643a",
				UpgradeTarget: "fc32ae4",
				LastHeartbeat: rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned: false,
		},
		{
			name:     "not upgrading at all",
			entry:    RegistryEntry{GitHash: "c11643a"},
			orphaned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateOrphanedUpgrade(&tc.entry, fixedSweepNow)
			if got.orphaned != tc.orphaned {
				t.Fatalf("orphaned = %v, want %v (reason %q)", got.orphaned, tc.orphaned, got.reason)
			}
			if got.orphaned && got.reason == "" {
				t.Fatal("an orphaned verdict must carry a reason for the log")
			}
		})
	}
}

// TestSweepOrphanedUpgradesClearsAndReArms asserts the two halves of the fix:
// the false in-flight flag goes away, AND the upgrade is still delivered. The
// second half is the one that matters — clearing alone would leave a
// hub-managed hive silently stranded on the old SHA.
func TestSweepOrphanedUpgradesClearsAndReArms(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{
		{
			ID:               "orphaned-hive",
			Upgrading:        true,
			UpgradeStartedAt: time.Now().Add(-68 * time.Minute),
			GitHash:          "c11643a",
			UpgradeTarget:    "fc32ae4",
			LastHeartbeat:    rfc3339(time.Now().Add(-30 * time.Second)),
		},
		{
			ID:               "healthy-in-flight",
			Upgrading:        true,
			UpgradeStartedAt: time.Now().Add(-2 * time.Minute),
			GitHash:          "c11643a",
			UpgradeTarget:    "fc32ae4",
			LastHeartbeat:    rfc3339(time.Now().Add(-10 * time.Second)),
		},
	}

	s.sweepOrphanedUpgrades()

	if s.registry.Hives[0].Upgrading {
		t.Error("orphaned hive should have had Upgrading cleared")
	}
	if got := s.heartbeatUpgrade["orphaned-hive"]; got != "fc32ae4" {
		t.Errorf("orphaned hive must be re-armed for delivery, heartbeatUpgrade = %q, want %q", got, "fc32ae4")
	}
	if s.registry.Hives[0].UpgradeTarget != "fc32ae4" {
		t.Error("UpgradeTarget must be preserved for observability")
	}
	if !s.registry.Hives[1].Upgrading {
		t.Error("a genuinely in-flight upgrade must not be cleared")
	}
}

// TestOrphanedUpgradeThresholdDerivesFromStaleUpgradeTimeout guards the
// constant relationship: the clear threshold must stay tied to — and strictly
// later than — the threshold at which the hub merely WARNS, so a slow upgrade
// is never cancelled at the moment it is first called stuck.
func TestOrphanedUpgradeThresholdDerivesFromStaleUpgradeTimeout(t *testing.T) {
	if orphanedUpgradeClearAfter <= staleUpgradeTimeout {
		t.Fatalf("clear threshold %v must be later than the warn threshold %v",
			orphanedUpgradeClearAfter, staleUpgradeTimeout)
	}
	if want := staleUpgradeTimeout + orphanedUpgradeGrace; orphanedUpgradeClearAfter != want {
		t.Fatalf("clear threshold %v must be derived from staleUpgradeTimeout, want %v",
			orphanedUpgradeClearAfter, want)
	}
}

// TestSweepEscalatesAfterRepeatedFailures asserts the bounded-retry behaviour
// that complements #2327. A hive that is structurally unable to advance — the
// floating-tag case, where the pod re-pulls its mutable tag and lands on a
// build that is neither the old SHA nor the target — must eventually be
// reported as a fault instead of being cleared and re-armed forever.
func TestSweepEscalatesAfterRepeatedFailures(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "never-lands",
		GitHash:       "c11643a",
		UpgradeTarget: "fc32ae4",
	}}

	// Each cycle re-latches the flag exactly as a re-armed upgrade would, so
	// the hive presents to the sweep as orphaned again every time.
	for i := 1; i <= maxOrphanedUpgradeSweeps; i++ {
		h := &s.registry.Hives[0]
		h.Upgrading = true
		h.UpgradeStartedAt = time.Now().Add(-68 * time.Minute)
		h.LastHeartbeat = rfc3339(time.Now().Add(-30 * time.Second))
		s.sweepOrphanedUpgrades()

		if got := s.registry.Hives[0].OrphanedUpgradeSweeps; got != i {
			t.Fatalf("after cycle %d, OrphanedUpgradeSweeps = %d, want %d", i, got, i)
		}
	}

	h := s.registry.Hives[0]
	if !h.UpgradeFailed {
		t.Errorf("after %d sweeps the hive must be marked UpgradeFailed", maxOrphanedUpgradeSweeps)
	}
	if h.UpgradeError == "" {
		t.Error("an exhausted upgrade must carry a human-readable cause")
	}
	if h.UpgradeFailedAt.IsZero() {
		t.Error("an exhausted upgrade must record when it was abandoned")
	}
	if _, armed := s.heartbeatUpgrade["never-lands"]; armed {
		t.Error("an exhausted upgrade must NOT stay armed for delivery")
	}
	if h.Upgrading {
		t.Error("an exhausted upgrade must not still claim to be in flight")
	}
}

// TestSweepBelowBudgetStillReArms guards that escalation does not fire early:
// under the budget the #2327 behaviour (clear AND re-arm) must be preserved
// unchanged, with no failure recorded.
func TestSweepBelowBudgetStillReArms(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:               "first-orphan",
		Upgrading:        true,
		UpgradeStartedAt: time.Now().Add(-68 * time.Minute),
		GitHash:          "c11643a",
		UpgradeTarget:    "fc32ae4",
		LastHeartbeat:    rfc3339(time.Now().Add(-30 * time.Second)),
	}}

	s.sweepOrphanedUpgrades()

	h := s.registry.Hives[0]
	if h.UpgradeFailed {
		t.Error("a single orphan sweep must not mark the hive failed")
	}
	if got := s.heartbeatUpgrade["first-orphan"]; got != "fc32ae4" {
		t.Errorf("below the budget the upgrade must still be re-armed, got %q", got)
	}
}

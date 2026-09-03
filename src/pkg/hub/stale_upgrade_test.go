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
		name      string
		entry     RegistryEntry
		latestSHA string
		orphaned  bool
		// converged distinguishes a clear that means "the upgrade LANDED, only
		// the latch was stale" from one that means "the attempt was abandoned".
		// The sweep re-arms and spends retry budget only on the latter.
		converged bool
	}{
		{
			// The floating-tag loop this fix closes: a hive tracking a mutable
			// …-latest tag reports the CURRENT latest build, which is a different
			// commit than the specific UpgradeTarget the hub armed. The old
			// exact-target test called this orphaned and swept/re-rolled it every
			// cycle; with the image ref known to be mutable and the reported SHA
			// equal to the branch-latest, it is converged, not orphaned.
			name: "not orphaned: floating-tag hive already on branch-latest (different from armed target)",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "9999abc",
				UpgradeTarget:    "fc32ae4",
				ImageRef:         "ghcr.io/hivecommons/hive:v4-latest",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			latestSHA: "9999abc",
			orphaned:  false,
		},
		{
			// A floating-tag hive that is NOT yet at latest is still a real
			// orphan — mutability alone is not a free pass, only mutability PLUS
			// being on the current build.
			name: "orphaned: floating-tag hive behind branch-latest",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				ImageRef:         "ghcr.io/hivecommons/hive:v4-latest",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			latestSHA: "9999abc",
			orphaned:  true,
		},
		{
			// A COMMIT-PINNED hive on branch-latest is unaffected by the floating
			// short-circuit: its tag resolves to one build, so "did it reach the
			// target" stays the governing question. Here it is still on the old
			// SHA, so it remains a genuine orphan.
			name: "orphaned: commit-pinned hive on old SHA even though branch-latest advanced",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				ImageRef:         "ghcr.io/hivecommons/hive:c11643a",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			latestSHA: "9999abc",
			orphaned:  true,
		},
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
			// Silence is only exculpatory while a restart could plausibly still
			// be in flight — i.e. under silentUpgradeDeadline.
			name: "in flight: silent since the upgrade was instructed (pod restarting)",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: fixedSweepNow.Add(-silentUpgradeDeadline + 5*time.Minute),
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-silentUpgradeDeadline)),
			},
			orphaned: false,
		},
		{
			// The z-aiops cohort wedge: the pod died mid-upgrade (crash loop /
			// failed pull / unreachable cluster), never came back, never
			// heartbeated again — and the evidence-only rule left the row
			// spinning "Upgrading" for 1.9h and counting. Past
			// silentUpgradeDeadline the silence IS the evidence.
			name: "orphaned: silent past the hard deadline (pod died mid-upgrade)",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: fixedSweepNow.Add(-silentUpgradeDeadline - time.Minute),
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-silentUpgradeDeadline - 2*time.Minute)),
			},
			orphaned: true,
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
			// The z-mlz-manager fixture: latched past the clear threshold, spoke
			// alive and heartbeating, and already ON the SHA it was asked for.
			// The upgrade completed; only the latch is stale, so it is cleared as
			// converged. Waiting for the heartbeat path here is what left the
			// spinner and the old SHA on screen for 35m+ with nothing in flight.
			name: "converged: spoke already reports the target and the latch is stale",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: started,
				GitHash:          "fc32ae4",
				UpgradeTarget:    "fc32ae4",
				LastHeartbeat:    rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned:  true,
			converged: true,
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
			name: "not orphaned: never heartbeated, under the silent deadline",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: fixedSweepNow.Add(-silentUpgradeDeadline + 5*time.Minute),
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
			},
			orphaned: false,
		},
		{
			// Never heartbeated at all AND past the deadline: same verdict as a
			// spoke that went silent — nothing that was going to report back
			// still exists.
			name: "orphaned: never heartbeated and past the silent deadline",
			entry: RegistryEntry{
				Upgrading:        true,
				UpgradeStartedAt: fixedSweepNow.Add(-silentUpgradeDeadline - time.Minute),
				GitHash:          "c11643a",
				UpgradeTarget:    "fc32ae4",
			},
			orphaned: true,
		},
		{
			// The ibm-alchemy live wedge: Upgrading latched with a ZERO
			// UpgradeStartedAt (0001-01-01), which the dashboard rendered as
			// "Upgrading 17755944h28m". A zero start can never be a fresh upgrade
			// (every set-site stamps time.Now()), so with a live spoke still on the
			// old SHA it must be cleared, not left wedged forever.
			name: "orphaned: zero start time with a live spoke on the old SHA (ibm-alchemy wedge)",
			entry: RegistryEntry{
				Upgrading:     true,
				GitHash:       "c11643a",
				UpgradeTarget: "fc32ae4",
				LastHeartbeat: rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned: true,
		},
		{
			// A zero start is still only cleared on the same liveness evidence as a
			// real stale upgrade. Never heartbeated ⇒ we cannot prove the attempt is
			// gone, so we do not clear even with a zero timestamp.
			name: "not orphaned: zero start time but the spoke never heartbeated",
			entry: RegistryEntry{
				Upgrading:     true,
				GitHash:       "c11643a",
				UpgradeTarget: "fc32ae4",
			},
			orphaned: false,
		},
		{
			// Zero start, alive, and the spoke already reports the target SHA: the
			// upgrade CONVERGED and only the latch is stale, so the sweep clears it.
			//
			// This case used to expect orphaned=false, deferring to "the heartbeat
			// path clears it". That deferral is the z-mlz-manager wedge: the
			// completion chain in server.go fires on a state TRANSITION, and once
			// the entry already records the target SHA no later beat carries one.
			// Both sides waited for the other and the spinner ran forever. Clearing
			// here is safe because liveness is already proven — a mid-restart pod is
			// not heartbeating — and `converged` keeps it out of the re-arm and
			// retry-budget paths that a genuine abandoned attempt takes.
			name: "converged: zero start time and the spoke already reports the target",
			entry: RegistryEntry{
				Upgrading:     true,
				GitHash:       "fc32ae4",
				UpgradeTarget: "fc32ae4",
				LastHeartbeat: rfc3339(fixedSweepNow.Add(-30 * time.Second)),
			},
			orphaned:  true,
			converged: true,
		},
		{
			name:     "not upgrading at all",
			entry:    RegistryEntry{GitHash: "c11643a"},
			orphaned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateOrphanedUpgrade(&tc.entry, fixedSweepNow, tc.latestSHA)
			if got.orphaned != tc.orphaned {
				t.Fatalf("orphaned = %v, want %v (reason %q)", got.orphaned, tc.orphaned, got.reason)
			}
			if got.orphaned && got.reason == "" {
				t.Fatal("an orphaned verdict must carry a reason for the log")
			}
			if got.converged != tc.converged {
				t.Fatalf("converged = %v, want %v (reason %q)", got.converged, tc.converged, got.reason)
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

// TestSweepDoesNotReRollFloatingTagAtLatest is the acceptance test for the
// perpetual floating-tag upgrade loop. A hive tracking a mutable …-latest tag,
// already running the branch-latest build, must NOT be swept: not re-armed for
// delivery, not counted toward the fault budget, and — after repeated cycles —
// never marked UpgradeFailed. Left unfixed this hive was re-armed and
// rolloutRestart-ed every staleUpgradeTimeout, restarting its spoke pod forever,
// then falsely reported as a permanent upgrade failure.
func TestSweepDoesNotReRollFloatingTagAtLatest(t *testing.T) {
	latestSHAMu.Lock()
	prev := latestSHAByBranch["v4"]
	latestSHAByBranch["v4"] = branchSHAInfo{SHA: "9999abc"}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestSHAByBranch["v4"] = prev
		latestSHAMu.Unlock()
	})

	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "floating-at-latest",
		GitBranch:     "v4",
		GitHash:       "9999abc", // == branch-latest
		UpgradeTarget: "fc32ae4", // an older armed commit the tag never lands on
		ImageRef:      "ghcr.io/hivecommons/hive:v4-latest",
	}}

	// Run more cycles than the fault budget: a broken hive would be marked
	// UpgradeFailed by now. A converged floating-tag hive must survive all of them.
	for i := 0; i < maxOrphanedUpgradeSweeps+2; i++ {
		s.registry.Hives[0].Upgrading = true
		s.registry.Hives[0].UpgradeStartedAt = time.Now().Add(-68 * time.Minute)
		s.registry.Hives[0].LastHeartbeat = rfc3339(time.Now().Add(-30 * time.Second))
		s.sweepOrphanedUpgrades()

		if _, armed := s.heartbeatUpgrade["floating-at-latest"]; armed {
			t.Fatalf("cycle %d: floating-tag hive at latest must NOT be re-armed for a rollout", i)
		}
		if s.registry.Hives[0].OrphanedUpgradeSweeps != 0 {
			t.Fatalf("cycle %d: converged floating-tag hive must not accrue orphan sweeps, got %d",
				i, s.registry.Hives[0].OrphanedUpgradeSweeps)
		}
	}
	if s.registry.Hives[0].UpgradeFailed {
		t.Error("a floating-tag hive at latest must never be reported as a permanent upgrade failure")
	}
}

// TestSweepUnwedgesZeroStartTimestamp is the anti-regression test for the live
// ibm-alchemy wedge: a hive latched Upgrading=true with a ZERO UpgradeStartedAt
// (0001-01-01) was NEVER swept, because the old evaluateOrphanedUpgrade bailed
// on IsZero() before it ever looked at liveness evidence. It stayed "Upgrading"
// forever with a 17.7M-hour counter and blocked any fresh upgrade from being
// recognised. The sweep must now clear such a hive (given a live spoke still on
// the old SHA) and re-arm delivery so it can upgrade again.
func TestSweepUnwedgesZeroStartTimestamp(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "ibm-alchemy-wedged",
		Upgrading:     true,
		GitHash:       "c11643a",
		UpgradeTarget: "fc32ae4",
		LastHeartbeat: rfc3339(time.Now().Add(-30 * time.Second)),
		// UpgradeStartedAt left zero on purpose — the wedge itself.
	}}
	if !s.registry.Hives[0].UpgradeStartedAt.IsZero() {
		t.Fatal("precondition: the wedged hive must carry a zero UpgradeStartedAt")
	}

	s.sweepOrphanedUpgrades()

	if s.registry.Hives[0].Upgrading {
		t.Error("a zero-timestamp wedged upgrade must be cleared, not left latched forever")
	}
	if got := s.heartbeatUpgrade["ibm-alchemy-wedged"]; got != "fc32ae4" {
		t.Errorf("the un-wedged hive must be re-armed for delivery, got %q", got)
	}
}

// TestSweepConvergedPathClearsWithoutReArmOrBudget is the integration test for
// the converged path added in #3718: when a hive has already reached (or
// surpassed) its UpgradeTarget, the sweep must clear the stale Upgrading latch
// WITHOUT re-arming heartbeatUpgrade and WITHOUT burning retry budget. This
// guards against the z-mlz-manager wedge where both the heartbeat completion
// chain and the sweep deferred to each other, leaving the spinner running
// forever on a hive that had in fact upgraded successfully.
func TestSweepConvergedPathClearsWithoutReArmOrBudget(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	// Pre-arm a heartbeat instruction that should be removed by the converged clear.
	s.heartbeatUpgrade["converged-hive"] = "fc32ae4"

	s.registry.Hives = []RegistryEntry{{
		ID:               "converged-hive",
		Upgrading:        true,
		UpgradeStartedAt: time.Now().Add(-68 * time.Minute),
		GitHash:          "fc32ae4", // already ON the target
		UpgradeTarget:    "fc32ae4",
		LastHeartbeat:    rfc3339(time.Now().Add(-30 * time.Second)),
		// Simulate a hive that was previously swept once as orphaned before
		// it landed the upgrade. The converged path must reset this.
		OrphanedUpgradeSweeps: 1,
	}}

	s.sweepOrphanedUpgrades()

	h := s.registry.Hives[0]

	// 1. Upgrading latch must be cleared.
	if h.Upgrading {
		t.Error("converged hive must have Upgrading cleared")
	}
	// 2. UpgradeTarget must be cleared (the upgrade landed; there is nothing to
	//    preserve for observability — unlike the orphaned path which keeps it).
	if h.UpgradeTarget != "" {
		t.Errorf("converged clear must reset UpgradeTarget, got %q", h.UpgradeTarget)
	}
	// 3. UpgradeStartedAt must be zeroed.
	if !h.UpgradeStartedAt.IsZero() {
		t.Error("converged clear must zero UpgradeStartedAt")
	}
	// 4. OrphanedUpgradeSweeps must be reset so prior sweeps don't count toward
	//    the fault budget on a FUTURE upgrade.
	if h.OrphanedUpgradeSweeps != 0 {
		t.Errorf("converged clear must reset OrphanedUpgradeSweeps, got %d", h.OrphanedUpgradeSweeps)
	}
	// 5. No UpgradeFailed state.
	if h.UpgradeFailed {
		t.Error("converged clear must not mark the hive as failed")
	}
	if h.UpgradeError != "" {
		t.Errorf("converged clear must not leave an error, got %q", h.UpgradeError)
	}
	if !h.UpgradeFailedAt.IsZero() {
		t.Error("converged clear must zero UpgradeFailedAt")
	}
	// 6. heartbeatUpgrade must be REMOVED, not re-armed: re-arming would
	//    re-instruct an upgrade that already happened.
	if _, armed := s.heartbeatUpgrade["converged-hive"]; armed {
		t.Error("converged hive must NOT be re-armed in heartbeatUpgrade — the upgrade already landed")
	}
}

// TestSweepConvergedNeverEscalatesToFault guards against the most dangerous
// regression: a hive that repeatedly converges (e.g., on successive upgrades
// where the heartbeat path is slow to clear the latch) must NEVER trip the
// maxOrphanedUpgradeSweeps fault escalation. Each converged clear resets the
// counter, so even maxOrphanedUpgradeSweeps+N cycles produce no UpgradeFailed.
func TestSweepConvergedNeverEscalatesToFault(t *testing.T) {
	s := &HubServer{
		logger:           slog.Default(),
		heartbeatUpgrade: make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{{
		ID:            "repeat-converger",
		GitHash:       "fc32ae4",
		UpgradeTarget: "fc32ae4",
	}}

	for i := 0; i < maxOrphanedUpgradeSweeps+2; i++ {
		h := &s.registry.Hives[0]
		h.Upgrading = true
		h.UpgradeStartedAt = time.Now().Add(-68 * time.Minute)
		h.GitHash = "fc32ae4"
		h.UpgradeTarget = "fc32ae4"
		h.LastHeartbeat = rfc3339(time.Now().Add(-30 * time.Second))

		s.sweepOrphanedUpgrades()

		if s.registry.Hives[0].OrphanedUpgradeSweeps != 0 {
			t.Fatalf("cycle %d: converged sweep must reset OrphanedUpgradeSweeps, got %d",
				i, s.registry.Hives[0].OrphanedUpgradeSweeps)
		}
		if _, armed := s.heartbeatUpgrade["repeat-converger"]; armed {
			t.Fatalf("cycle %d: converged hive must not be re-armed", i)
		}
	}
	if s.registry.Hives[0].UpgradeFailed {
		t.Error("a hive that converges on every cycle must never be reported as a permanent upgrade failure")
	}
}

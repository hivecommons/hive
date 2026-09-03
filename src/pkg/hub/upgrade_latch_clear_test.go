package hub

import (
	"strings"
	"testing"
	"time"
)

// ============================================================================
// Stale "Upgrading" latch — every SET must have a CLEAR.
// ============================================================================
//
// The live wedge these cover (z-mlz-manager, 2026-08-13): the hub armed an
// upgrade to 815d7e4, the spoke rolled onto ghcr.io/hivecommons/hive:v4-latest
// and came back ready and heartbeating, and the hub still showed the spinner,
// "Starting up after upgrade" and the OLD sha 35+ minutes later. Nothing on the
// fleet was actually mid-upgrade.
//
// The deadlock was in evaluateOrphanedUpgrade: a spoke proven alive and proven
// AT-OR-AHEAD of its target returned orphaned=false on the reasoning that "the
// heartbeat path clears it" — while the heartbeat completion chain only fires
// on a state TRANSITION it recognises. When the entry already records the
// target sha, later beats carry no transition, so both sides deferred to each
// other forever.

// liveWedgeEntry builds the z-mlz-manager fixture: latched-upgrading well past
// the clear threshold, with a spoke that has heartbeated SINCE the instruction.
func liveWedgeEntry(now time.Time, gitHash, target string) *RegistryEntry {
	return &RegistryEntry{
		ID:               "z-mlz-manager",
		Upgrading:        true,
		UpgradeTarget:    target,
		GitHash:          gitHash,
		UpgradeStartedAt: now.Add(-35 * time.Minute),
		LastHeartbeat:    now.Add(-2 * time.Minute).Format(time.RFC3339),
		ImageRef:         "ghcr.io/hivecommons/hive:v4-latest",
		GitBranch:        "v4",
	}
}

// Case 1: the spoke reports the EXACT sha the hub asked for -> latch clears.
//
// This is the live z-mlz-manager state (GitHash == UpgradeTarget == 815d7e4)
// and is precisely what the old code refused to clear.
func TestOrphanSweepClearsWhenSpokeReportsExactRequestedSHA(t *testing.T) {
	now := time.Now()
	ev := evaluateOrphanedUpgrade(liveWedgeEntry(now, "815d7e4", "815d7e4"), now, "")
	if !ev.orphaned {
		t.Fatalf("spoke is alive and running the exact requested sha, but the latch was not cleared (reason=%q)", ev.reason)
	}
	if !ev.converged {
		t.Errorf("reaching the requested sha is a COMPLETED upgrade; converged = false, reason=%q", ev.reason)
	}
	if !strings.Contains(ev.reason, "815d7e4") {
		t.Errorf("reason should name the sha, got %q", ev.reason)
	}
}

// Case 2: the spoke reports a DIFFERENT, NEWER sha than the one requested ->
// the latch STILL clears. The hub asked for X, a newer merge landed first, the
// floating v4-latest tag delivered Y, and a latch keyed on X will never see X.
func TestOrphanSweepClearsWhenSpokeRolledToNewerSHAThanRequested(t *testing.T) {
	now := time.Now()
	target, reported := "815d7e4", "a2b2c2c"

	// Seed the ancestry cache so the cache-only check can answer "ahead"
	// without any network call (commit_order.go resolves in the background).
	commitOrderMu.Lock()
	commitOrderCache[commitOrderKey{target: target, reported: reported}] = true
	commitOrderMu.Unlock()
	defer func() {
		commitOrderMu.Lock()
		delete(commitOrderCache, commitOrderKey{target: target, reported: reported})
		commitOrderMu.Unlock()
	}()

	ev := evaluateOrphanedUpgrade(liveWedgeEntry(now, reported, target), now, "")
	if !ev.orphaned {
		t.Fatalf("spoke rolled to %s, ahead of the requested %s, but the latch was not cleared (reason=%q)",
			reported, target, ev.reason)
	}
	if !ev.converged {
		t.Errorf("landing ahead of the target is a COMPLETED upgrade; converged = false, reason=%q", ev.reason)
	}
}

// Case 3 (POSITIVE CONTROL): the spoke is still on the OLD sha, strictly BEHIND
// the target -> the latch REMAINS an un-upgraded orphan, never "converged".
//
// Without this, an "always clear" implementation would satisfy cases 1 and 2
// while destroying the distinction the sweep exists to draw.
func TestOrphanSweepDoesNotReportConvergedWhenSpokeStillOnOldSHA(t *testing.T) {
	now := time.Now()
	target, reported := "aaaaaaa", "815d7e4"

	// Explicitly cache "not ahead" so the result cannot depend on an
	// unresolved pair happening to default to false.
	commitOrderMu.Lock()
	commitOrderCache[commitOrderKey{target: target, reported: reported}] = false
	commitOrderMu.Unlock()
	defer func() {
		commitOrderMu.Lock()
		delete(commitOrderCache, commitOrderKey{target: target, reported: reported})
		commitOrderMu.Unlock()
	}()

	ev := evaluateOrphanedUpgrade(liveWedgeEntry(now, reported, target), now, "")
	if ev.converged {
		t.Fatalf("spoke is BEHIND the target (%s < %s); this is an abandoned attempt, not a converged one (reason=%q)",
			reported, target, ev.reason)
	}
	if !ev.orphaned {
		t.Fatalf("an alive spoke still on the old sha long past the threshold is an orphan, got orphaned=false")
	}
	if !strings.Contains(ev.reason, "no upgrade attempt in flight") {
		t.Errorf("expected the abandoned-attempt reason, got %q", ev.reason)
	}
}

// A genuinely in-flight upgrade must never be cancelled: a restarting pod is
// not heartbeating, so a spoke silent since the instruction stays latched even
// though it is nominally "at" the target.
func TestOrphanSweepLeavesGenuineInFlightUpgradeAlone(t *testing.T) {
	now := time.Now()
	e := liveWedgeEntry(now, "815d7e4", "815d7e4")
	// Silent since the upgrade was instructed — the pod is rolling.
	e.LastHeartbeat = now.Add(-40 * time.Minute).Format(time.RFC3339)
	ev := evaluateOrphanedUpgrade(e, now, "")
	if ev.orphaned {
		t.Errorf("a spoke silent since the instruction is mid-restart and must not be cleared (reason=%q)", ev.reason)
	}
}

// A converged clear must not burn orphan-sweep retry budget, must not re-arm
// the instruction, and must wipe the latch fields — otherwise three successful
// upgrades would tip a healthy hive into a permanent false UpgradeFailed.
func TestSweepConvergedClearsLatchWithoutRearmingOrBurningBudget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	now := time.Now()
	e := liveWedgeEntry(now, "815d7e4", "815d7e4")
	e.OrphanedUpgradeSweeps = 2 // one short of maxOrphanedUpgradeSweeps
	s.registry.Hives = []RegistryEntry{*e}
	s.heartbeatUpgrade["z-mlz-manager"] = "815d7e4"

	s.sweepOrphanedUpgrades()

	s.mu.RLock()
	got := s.registry.Hives[0]
	_, stillArmed := s.heartbeatUpgrade["z-mlz-manager"]
	s.mu.RUnlock()

	if got.Upgrading {
		t.Error("Upgrading latch should be cleared")
	}
	if got.UpgradeTarget != "" {
		t.Errorf("UpgradeTarget should be cleared, got %q", got.UpgradeTarget)
	}
	if !got.UpgradeStartedAt.IsZero() {
		t.Error("UpgradeStartedAt should be zeroed so the next upgrade times from zero")
	}
	if got.UpgradeFailed {
		t.Errorf("a converged upgrade must never be reported as failed: %q", got.UpgradeError)
	}
	if got.OrphanedUpgradeSweeps != 0 {
		t.Errorf("converged clear must reset the retry budget, got %d", got.OrphanedUpgradeSweeps)
	}
	if stillArmed {
		t.Error("converged clear must drop the armed instruction; re-delivering would roll a healthy pod")
	}
}

// ============================================================================
// Filter pill vs row badge — ONE predicate.
// ============================================================================

// The "Upgrading" facet under-reported because the pill tested only
// h.upgrading while the row OR'd three sources. Assert from the embedded JS
// that both call sites route through the single shared helper.
func TestUpgradingFilterAndRowBadgeShareOnePredicate(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function hiveIsUpgradingNow(") {
		t.Fatal("shared predicate hiveIsUpgradingNow is missing")
	}

	// The pill's classifier must delegate, not re-derive from h.upgrading.
	stateFn := jsFunctionBody(t, "hiveUpgradeState")
	if !strings.Contains(stateFn, "hiveIsUpgradingNow(h)") {
		t.Errorf("hiveUpgradeState must delegate to hiveIsUpgradingNow, body:\n%s", stateFn)
	}
	if strings.Contains(stateFn, "if (h.upgrading)") {
		t.Errorf("hiveUpgradeState still tests h.upgrading directly — that is the under-reporting bug:\n%s", stateFn)
	}

	// The row badge must assign isUpgrading from the same helper rather than
	// rebuilding the OR chain inline.
	if !strings.Contains(dashboardHTML, "var isUpgrading = hiveIsUpgradingNow(h, branchName, branchLatest);") {
		t.Error("the row badge must compute isUpgrading via hiveIsUpgradingNow")
	}
	if strings.Contains(dashboardHTML, "var isUpgrading = isSwitching ||") {
		t.Error("the row badge still rebuilds its own OR chain; the pill cannot match it")
	}

	// The shared helper must actually cover all three sources the row showed,
	// or unifying on it would fix the disagreement by dropping badges.
	body := jsFunctionBody(t, "hiveIsUpgradingNow")
	for _, want := range []string{"isSwitching", "_upgradingHives", "h.upgrading"} {
		if !strings.Contains(body, want) {
			t.Errorf("shared predicate drops the %q source:\n%s", want, body)
		}
	}
	// And it must keep the row's own suppressions, or the pill would now
	// over-report where the row shows nothing.
	for _, want := range []string{"isCurrent", "latestUnknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("shared predicate drops the %q suppression:\n%s", want, body)
		}
	}
}

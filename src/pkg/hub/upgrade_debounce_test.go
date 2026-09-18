package hub

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests below assert the BEHAVIOUR #5391 asks for, not that a particular
// function was called: a burst of merges must produce ONE roll at the NEWEST
// SHA, a quiet branch must still upgrade, and the paths an operator drives must
// not be delayed at all.

const (
	testDebounce = 5 * time.Minute
	// Generous cap: the tests in this file that are about the QUIET-window
	// behaviour must not trip the max hold. The cap has its own test.
	testMaxHold = 4 * time.Hour
)

// disableUpgradeDebounceForTest turns the merge-driven debounce (#5391) off for
// the duration of a test, restoring the historical roll-on-the-first-cycle
// behaviour.
//
// It exists for the same reason registryBehind() sets LastHeartbeat: a test
// whose subject is ELIGIBILITY (the assignment latch, the wave bound, the
// collectibility refusal) drives triggerAutoUpgrades() once and asserts an
// upgrade was armed. Debounce is an unrelated gate for those tests — leaving it
// on would make every one of them fail on timing and stop testing what it is
// named for.
//
// Tests whose subject IS the debounce must NOT use this. They live in this file
// and exercise shouldDebounceAutoUpgrade directly across a sequence of cycles.
func disableUpgradeDebounceForTest(t *testing.T) {
	t.Helper()
	t.Setenv("HIVE_UPGRADE_DEBOUNCE_SECONDS", "-1")
	resetScaleSettingsForTest()
	if got := autoUpgradeDebounceInterval(); got != 0 {
		t.Fatalf("failed to disable debounce for this test: interval = %v", got)
	}
}

// driveMerges replays a sequence of (target, time) observations through the
// debounce gate exactly as triggerAutoUpgrades does — carrying the persisted
// state forward across cycles — and returns every target that was actually
// allowed to roll, plus the collapse count reported with each.
//
// This is the observable behaviour under test: the list of rolls a spoke would
// actually perform. A test that asserted on internal state instead could pass
// while the fleet still rolled eleven times.
type roll struct {
	target    string
	collapsed int
}

func driveMerges(observations []struct {
	target string
	at     time.Time
}, interval, maxHold time.Duration) []roll {
	var state autoUpgradeDebounceState
	var rolls []roll
	for _, o := range observations {
		d := shouldDebounceAutoUpgrade(state, o.target, interval, maxHold, o.at)
		if d.Allowed {
			rolls = append(rolls, roll{target: o.target, collapsed: d.Collapsed})
			state = autoUpgradeDebounceState{} // the fire path clears the record
			continue
		}
		state = d.State
	}
	return rolls
}

// TestMergeBurstCollapsesToOneRollAtNewestSHA is the core claim of #5391.
//
// It replays the measured incident: a run of merges landing inside the quiet
// window, observed on the hub's 2-minute poll cadence. The fleet rolled once
// per merge. It must now roll ONCE, and land on the LAST SHA, not the first.
func TestMergeBurstCollapsesToOneRollAtNewestSHA(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 45, 0, 0, time.UTC)
	// Five merges, each seen on a later poll, all within the 5m window of the
	// one before it — so the window keeps being re-armed and never elapses.
	obs := []struct {
		target string
		at     time.Time
	}{
		{"e00214b", base},
		{"d12ed62", base.Add(2 * time.Minute)},
		{"bcab144", base.Add(4 * time.Minute)},
		{"7d0afff", base.Add(6 * time.Minute)},
		{"db613df", base.Add(8 * time.Minute)},
		// Branch goes quiet: the newest target is re-observed on later polls
		// until the window finally elapses.
		{"db613df", base.Add(10 * time.Minute)},
		{"db613df", base.Add(12 * time.Minute)},
		{"db613df", base.Add(14 * time.Minute)},
	}

	rolls := driveMerges(obs, testDebounce, testMaxHold)

	if len(rolls) != 1 {
		t.Fatalf("a burst of 5 merges must collapse into exactly ONE roll, got %d: %+v", len(rolls), rolls)
	}
	if rolls[0].target != "db613df" {
		t.Errorf("the single roll must land on the NEWEST target db613df, got %q", rolls[0].target)
	}
	// The roll must be able to say how many merges it absorbed — silent
	// batching is explicitly rejected by #5391.
	if rolls[0].collapsed != 4 {
		t.Errorf("roll must report the 4 superseded targets it collapsed, reported %d", rolls[0].collapsed)
	}
}

// TestBusyBranchIsNotStarvedByDebounce is the guard on the one real failure
// mode of a pure debounce: a branch that NEVER goes quiet would re-arm the
// window forever and the hive would never upgrade at all.
//
// This is not hypothetical for v4. Sampling the 100 merges from 2026-08-30
// 19:07Z to 2026-08-31 23:33Z, the MEDIAN inter-merge gap is 3.0 minutes and
// 63% of gaps are under 5 minutes — i.e. below the debounce interval. The
// max-hold cap is what converts "rolls once the branch goes quiet" into "rolls
// once the branch goes quiet, and in no case later than the cap".
//
// The test replays a relentless branch: a new merge every 2 minutes, never
// pausing, for two hours.
func TestBusyBranchIsNotStarvedByDebounce(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	const maxHold = 30 * time.Minute

	var obs []struct {
		target string
		at     time.Time
	}
	for i := 0; i < 60; i++ { // 60 merges x 2m = 2 hours, never quiet
		obs = append(obs, struct {
			target string
			at     time.Time
		}{fmt.Sprintf("sha%04d", i), base.Add(time.Duration(i) * 2 * time.Minute)})
	}

	rolls := driveMerges(obs, testDebounce, maxHold)

	if len(rolls) == 0 {
		t.Fatal("a permanently busy branch was STARVED — the hive would never upgrade at all")
	}
	// Bounded above: roughly one roll per max-hold period across the 2h run,
	// not one per merge. The old behaviour would have produced 60.
	if len(rolls) > 6 {
		t.Errorf("busy branch produced %d rolls in 2h; the cap should bound this near 2h/%v", len(rolls), maxHold)
	}
	// The cap must fire repeatedly across the run rather than once — otherwise
	// the hive converges once and then starves again.
	if len(rolls) < 2 {
		t.Errorf("expected the cap to fire repeatedly across 2 hours, got %d roll(s)", len(rolls))
	}
	// Each roll must land on a target that was current at the time, and the
	// LAST roll must be recent enough that the hive is not left far behind.
	if rolls[len(rolls)-1].target == "sha0000" {
		t.Error("the final roll landed on the very first SHA — the hive is not converging")
	}
}

// TestNoMergeCadenceStarvesTheHive sweeps a range of merge cadences and asserts
// that NONE of them starve the hive.
//
// This exists because a plausible-looking staleness rule very nearly shipped
// that did starve it. Keying "is this record stale" on ELAPSED TIME (older than
// maxHold) rather than on provenance meant that on any branch whose merge gap
// exceeded the poll step, the reset fired before the cap could, pushing the
// wait clock forward every single cycle so the cap never fired at all —
// measured at a 7-minute cadence: ZERO rolls in 4.5 hours, a total silent
// stall. A single-cadence test missed it because the 2-minute case still
// passed.
//
// Sweeping cadences is what catches that class of bug, so this test sweeps.
func TestNoMergeCadenceStarvesTheHive(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	const maxHold = 30 * time.Minute

	for _, step := range []time.Duration{
		time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute,
		7 * time.Minute, 11 * time.Minute, 13 * time.Minute,
	} {
		var obs []struct {
			target string
			at     time.Time
		}
		const n = 60
		for i := 0; i < n; i++ {
			obs = append(obs, struct {
				target string
				at     time.Time
			}{fmt.Sprintf("sha%04d", i), base.Add(time.Duration(i) * step)})
		}
		span := time.Duration(n-1) * step

		rolls := driveMerges(obs, testDebounce, maxHold)

		if len(rolls) == 0 {
			t.Errorf("merge cadence %v over %v STARVED the hive — zero rolls, it would never upgrade", step, span)
			continue
		}
		// Upper bound: never worse than one roll per merge, and in practice far
		// better. Expressed against the span so it holds for every cadence.
		if maxExpected := int(span/maxHold) + 2; len(rolls) > maxExpected {
			t.Errorf("cadence %v produced %d rolls over %v, want <= %d", step, len(rolls), span, maxExpected)
		}
		// It must also converge: the last roll cannot be the very first SHA.
		if rolls[len(rolls)-1].target == "sha0000" {
			t.Errorf("cadence %v: final roll is still the first SHA — not converging", step)
		}
	}
}

// TestQuietBranchStillUpgradesPromptly guards the cost of debouncing: a lone
// merge with nothing behind it must still roll, one window later — not never,
// and not only at some daily window.
func TestQuietBranchStillUpgradesPromptly(t *testing.T) {
	base := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC)
	obs := []struct {
		target string
		at     time.Time
	}{
		{"abc1234", base},
		{"abc1234", base.Add(2 * time.Minute)}, // still inside the window
		{"abc1234", base.Add(4 * time.Minute)}, // still inside the window
		{"abc1234", base.Add(6 * time.Minute)}, // window elapsed
	}

	rolls := driveMerges(obs, testDebounce, testMaxHold)

	if len(rolls) != 1 {
		t.Fatalf("a quiet branch must roll exactly once, got %d: %+v", len(rolls), rolls)
	}
	if rolls[0].target != "abc1234" {
		t.Errorf("roll target = %q, want abc1234", rolls[0].target)
	}
	if rolls[0].collapsed != 0 {
		t.Errorf("a lone merge collapsed nothing, want 0, got %d", rolls[0].collapsed)
	}
}

// TestDebounceDisabledRollsImmediately asserts the escape hatch restores the
// historical behaviour exactly: every observed target rolls, with no wait.
func TestDebounceDisabledRollsImmediately(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 45, 0, 0, time.UTC)
	obs := []struct {
		target string
		at     time.Time
	}{
		{"e00214b", base},
		{"d12ed62", base.Add(2 * time.Minute)},
		{"bcab144", base.Add(4 * time.Minute)},
	}

	rolls := driveMerges(obs, 0, testMaxHold)

	if len(rolls) != 3 {
		t.Fatalf("with debounce disabled every merge must roll, want 3, got %d: %+v", len(rolls), rolls)
	}
	for i, want := range []string{"e00214b", "d12ed62", "bcab144"} {
		if rolls[i].target != want {
			t.Errorf("roll %d target = %q, want %q", i, rolls[i].target, want)
		}
	}
}

// TestPendingTargetSurvivesHubRestart asserts constraint 2: an upgrade pending
// inside the quiet window must not be LOST when the hub restarts, and the wait
// must resume from the original arming rather than starting over.
//
// The restart is modelled the way it actually happens: the in-memory state is
// discarded and reconstructed from the fields persisted on the hive record.
func TestPendingTargetSurvivesHubRestart(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 45, 0, 0, time.UTC)

	// Arm the window.
	d := shouldDebounceAutoUpgrade(autoUpgradeDebounceState{}, "db613df", testDebounce, testMaxHold, base)
	if d.Allowed {
		t.Fatal("a freshly observed target must be held, not rolled immediately")
	}

	// The hub persists the pending state and then restarts. Everything not on
	// the hive record is gone; rebuild only from the persisted fields.
	h := &SaaSHive{
		ID:                       "hive-restart-test",
		AutoUpgradePendingTarget: d.State.Target,
		AutoUpgradePendingSince:  d.State.ArmedAt,
		AutoUpgradeCollapsed:     d.State.Collapsed,
	}
	if h.AutoUpgradePendingTarget != "db613df" {
		t.Fatalf("pending target was not persisted, got %q", h.AutoUpgradePendingTarget)
	}
	recovered := autoUpgradeDebounceState{
		Target:    h.AutoUpgradePendingTarget,
		ArmedAt:   h.AutoUpgradePendingSince,
		Collapsed: h.AutoUpgradeCollapsed,
	}

	// Immediately after the restart the window has NOT elapsed — still held.
	if got := shouldDebounceAutoUpgrade(recovered, "db613df", testDebounce, testMaxHold, base.Add(time.Minute)); got.Allowed {
		t.Error("restart must not cause an early roll while the window is still open")
	}

	// Once the ORIGINAL window elapses the upgrade fires. If the restart had
	// reset the clock this would still be holding.
	after := shouldDebounceAutoUpgrade(recovered, "db613df", testDebounce, testMaxHold, base.Add(testDebounce+time.Second))
	if !after.Allowed {
		t.Fatal("pending upgrade was LOST across the restart — it must still roll")
	}
	if after.Reason == "" {
		t.Error("decision must always carry a log-friendly reason")
	}
}

// TestShortAndFullSHADoNotReArmForever guards a failure mode that would be
// silent and total: if a stored short SHA and a reported longer SHA compared as
// DIFFERENT, every cycle would re-arm the window and the hive would never
// upgrade again. sameCommit tolerance is what prevents that.
func TestShortAndFullSHADoNotReArmForever(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 45, 0, 0, time.UTC)
	armed := autoUpgradeDebounceState{Target: "db613df", ArmedAt: base}

	// The same commit, reported at full length, must NOT look like a new merge.
	d := shouldDebounceAutoUpgrade(armed, "db613df1c0ffee0123456789abcdef0123456789", testDebounce, testMaxHold, base.Add(2*time.Minute))
	if d.Allowed {
		t.Fatal("window should still be open at +2m")
	}
	if d.State.Collapsed != 0 {
		t.Errorf("a length-only SHA difference must not count as a collapsed merge, got %d", d.State.Collapsed)
	}
	if !d.State.ArmedAt.Equal(base) {
		t.Error("a length-only SHA difference must not re-arm the window — the hive would never upgrade")
	}

	// And it fires on schedule rather than being deferred forever.
	if got := shouldDebounceAutoUpgrade(armed, "db613df1c0ffee0123456789abcdef0123456789", testDebounce, testMaxHold, base.Add(6*time.Minute)); !got.Allowed {
		t.Error("upgrade must fire once the window elapses despite the SHA length mismatch")
	}
}

// TestEmptyTargetClearsPendingState asserts a stale pending target cannot
// survive to fire against a branch that has since moved on.
func TestEmptyTargetClearsPendingState(t *testing.T) {
	base := time.Date(2026, 8, 31, 20, 45, 0, 0, time.UTC)
	armed := autoUpgradeDebounceState{Target: "db613df", ArmedAt: base, Collapsed: 3}

	d := shouldDebounceAutoUpgrade(armed, "", testDebounce, testMaxHold, base.Add(time.Minute))
	if d.Allowed {
		t.Error("an empty target must never roll")
	}
	if d.State.Target != "" {
		t.Errorf("an empty target must clear the pending record, still holds %q", d.State.Target)
	}
}

// TestManualAndPinnedUpgradesBypassDebounce asserts hard constraint 1 of
// #5391: debounce is for the AUTOMATIC merge-driven path only. An operator
// clicking "Upgrade now", a bulk upgrade, and a hard image pin must all still be
// immediate.
//
// Those three paths arm s.heartbeatUpgrade DIRECTLY — upgradeHiveHandler and
// saas_bulk.go write the map themselves, and a pin is delivered as
// h.UpgradeTarget through the stale-recovery branch, which sits ABOVE the
// `if !h.AutoUpgrade` gate and `continue`s. None of them reach the debounce
// gate, so none of them can be delayed by it.
//
// What this test pins is the OTHER half of that argument: that the gate itself
// is reached only when the hive is chasing latest automatically. It asserts on
// the source of triggerAutoUpgrades because the ordering — pin/manual arming
// happens before the auto gate — is precisely the property that keeps manual
// upgrades immediate, and a refactor that moved the debounce call above the
// AutoUpgrade gate would silently start delaying operator-driven upgrades while
// every behavioural test above still passed.
func TestManualAndPinnedUpgradesBypassDebounce(t *testing.T) {
	src, err := os.ReadFile("saas_upgrade.go")
	if err != nil {
		t.Fatalf("reading saas_upgrade.go: %v", err)
	}
	body := string(src)

	fn := strings.Index(body, "func (s *HubServer) triggerAutoUpgrades()")
	if fn < 0 {
		t.Fatal("triggerAutoUpgrades not found — this test must be re-pointed")
	}
	autoGate := strings.Index(body[fn:], "if !h.AutoUpgrade {")
	if autoGate < 0 {
		t.Fatal("the `if !h.AutoUpgrade` gate was not found inside triggerAutoUpgrades")
	}
	debounce := strings.Index(body[fn:], "shouldDebounceAutoUpgrade(")
	if debounce < 0 {
		t.Fatal("the debounce gate was not found inside triggerAutoUpgrades")
	}

	if debounce < autoGate {
		t.Error("debounce is evaluated BEFORE the `if !h.AutoUpgrade` gate: " +
			"manual, bulk and pinned upgrades would be delayed, violating #5391 constraint 1")
	}
}

// TestTriggerAutoUpgradesHoldsThenRollsOnce is the end-to-end wiring test: it
// drives the REAL triggerAutoUpgrades() sweep rather than the policy function,
// and asserts the observable outcome a spoke would experience.
//
// Without it the policy could be perfect while the sweep ignored it — which is
// exactly the defect #5391 describes, one layer up.
func TestTriggerAutoUpgradesHoldsThenRollsOnce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	// A short but non-zero window, so the test can step over it without sleeping.
	t.Setenv("HIVE_UPGRADE_DEBOUNCE_SECONDS", "300")
	resetScaleSettingsForTest()
	setLatestSHAForBranchForTest(t, "v4", "merge111")

	const id = "debounce-e2e"
	saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "dc1"})
	s := &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		heartbeatUpgrade: make(map[string]string),
		clusters:         map[string]ClusterConfig{"dc1": {ID: "dc1", InCluster: true}},
	}
	beat := time.Now().UTC().Format(time.RFC3339)
	s.registry.Hives = []RegistryEntry{{ID: id, GitBranch: "v4", GitHash: "oldsha", LastHeartbeat: beat}}

	armed := func() int {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return len(s.heartbeatUpgrade)
	}

	// Cycle 1: first sight of the merge. Must HOLD, not roll.
	s.triggerAutoUpgrades()
	if armed() != 0 {
		t.Fatal("a freshly merged target rolled immediately — debounce is not wired into triggerAutoUpgrades")
	}
	// The pending target must be persisted, or a hub restart here loses it.
	stored := loadSaaSHive(id)
	if stored == nil || stored.AutoUpgradePendingTarget != "merge111" {
		t.Fatalf("pending target was not persisted to the hive record, got %+v", stored)
	}

	// Cycle 2: a NEWER merge lands inside the window. Still no roll, and the
	// pending target must be REPLACED rather than a second roll queued.
	setLatestSHAForBranchForTest(t, "v4", "merge222")
	s.triggerAutoUpgrades()
	if armed() != 0 {
		t.Fatal("a second merge inside the window rolled — the burst was not collapsed")
	}
	stored = loadSaaSHive(id)
	if stored.AutoUpgradePendingTarget != "merge222" {
		t.Errorf("pending target = %q, want the NEWER merge222", stored.AutoUpgradePendingTarget)
	}
	if stored.AutoUpgradeCollapsed != 1 {
		t.Errorf("collapsed count = %d, want 1 superseded target", stored.AutoUpgradeCollapsed)
	}

	// The branch goes quiet. Backdate the arming to step past the window
	// without sleeping, then sweep again: now it must roll, exactly once, on
	// the NEWEST SHA.
	stored.AutoUpgradePendingSince = time.Now().Add(-10 * time.Minute)
	if err := saveSaaSHive(stored); err != nil {
		t.Fatalf("saving backdated hive: %v", err)
	}
	s.triggerAutoUpgrades()
	if armed() != 1 {
		t.Fatalf("after the quiet window the upgrade must roll, armed %d", armed())
	}
	s.mu.RLock()
	target := s.heartbeatUpgrade[id]
	s.mu.RUnlock()
	if target != "merge222" {
		t.Errorf("rolled to %q, want the newest SHA merge222", target)
	}
	// And the debounce record must be cleared so the next merge starts fresh.
	if after := loadSaaSHive(id); after.AutoUpgradePendingTarget != "" {
		t.Errorf("debounce record not cleared after the roll, still %q", after.AutoUpgradePendingTarget)
	}
}

// TestDebounceIntervalIsConfigurable asserts the interval is a named,
// overridable knob rather than a magic number, and that the documented default
// is what an unconfigured fleet gets.
func TestDebounceIntervalIsConfigurable(t *testing.T) {
	resetScaleSettingsForTest()
	t.Setenv("HIVE_UPGRADE_DEBOUNCE_SECONDS", "")
	if got := autoUpgradeDebounceInterval(); got != defaultAutoUpgradeDebounceInterval {
		t.Errorf("unconfigured interval = %v, want the documented default %v", got, defaultAutoUpgradeDebounceInterval)
	}

	t.Setenv("HIVE_UPGRADE_DEBOUNCE_SECONDS", "90")
	if got := autoUpgradeDebounceInterval(); got != 90*time.Second {
		t.Errorf("env override = %v, want 90s", got)
	}

	// A negative value is the explicit "disable debouncing" escape hatch.
	t.Setenv("HIVE_UPGRADE_DEBOUNCE_SECONDS", "-1")
	if got := autoUpgradeDebounceInterval(); got != 0 {
		t.Errorf("negative override must disable debounce (0), got %v", got)
	}
}

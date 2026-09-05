package hub

import (
	"log/slog"
	"testing"
	"time"
)

// These tests pin the max-hold pieces of the #5391 debounce that the burst and
// starvation tests in upgrade_debounce_test.go do not reach: the
// HIVE_UPGRADE_MAX_HOLD_SECONDS resolution in autoUpgradeMaxHold(), the cap
// firing on a QUIET cycle before the window has elapsed, and the
// persistUpgradeDebounceState round-trip (including its nil-hive and
// save-failure edges).

// TestAutoUpgradeMaxHoldEnvResolution pins the documented override
// conventions: 0/unset = built-in default, negative = cap disabled entirely,
// positive = that many seconds, and a value that does not parse is ignored
// rather than treated as disabled.
func TestAutoUpgradeMaxHoldEnvResolution(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset uses default", "", defaultAutoUpgradeMaxHold},
		{"zero means not set, uses default", "0", defaultAutoUpgradeMaxHold},
		{"positive overrides in seconds", "600", 10 * time.Minute},
		{"negative disables the cap", "-1", 0},
		{"unparseable is ignored, uses default", "soon", defaultAutoUpgradeMaxHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_UPGRADE_MAX_HOLD_SECONDS", tc.env)
			if got := autoUpgradeMaxHold(); got != tc.want {
				t.Errorf("autoUpgradeMaxHold() with env %q = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestMaxHoldFiresOnQuietCycleBeforeWindowElapses covers the quiet-path cap:
// a hive can arrive at the SAME-TARGET case with the total hold already past
// maxHold (the busy cycles consumed it), and it must roll on that cycle even
// though the quiet window has not elapsed. Without this branch a hive could be
// held past the cap by alternating busy and quiet cycles.
func TestMaxHoldFiresOnQuietCycleBeforeWindowElapses(t *testing.T) {
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	const (
		interval = 5 * time.Minute
		maxHold  = 10 * time.Minute
	)

	var state autoUpgradeDebounceState
	// Busy cycles: each new target re-arms the window but keeps FirstArmedAt.
	for i, o := range []struct {
		target string
		at     time.Time
	}{
		{"sha-a", base},
		{"sha-b", base.Add(4 * time.Minute)},
		{"sha-c", base.Add(8 * time.Minute)},
	} {
		d := shouldDebounceAutoUpgrade(state, o.target, interval, maxHold, o.at)
		if d.Allowed {
			t.Fatalf("cycle %d: rolled early (reason %q); cap should not have been reached yet", i, d.Reason)
		}
		state = d.State
	}
	if state.FirstArmedAt != base {
		t.Fatalf("FirstArmedAt = %v, want the original arm time %v", state.FirstArmedAt, base)
	}

	// Quiet cycle: same target, only 4m into a 5m window — but the total hold
	// is now 12m, past the 10m cap. It must fire NOW, reporting the collapse.
	d := shouldDebounceAutoUpgrade(state, "sha-c", interval, maxHold, base.Add(12*time.Minute))
	if !d.Allowed {
		t.Fatalf("quiet cycle past the cap held (reason %q); the cap must apply on the quiet path too", d.Reason)
	}
	if d.Collapsed != 2 {
		t.Errorf("Collapsed = %d, want 2 (sha-b and sha-c superseded a pending target)", d.Collapsed)
	}
}

// TestQuietPathHoldsWhenFirstArmedAtIsMissing pins the legacy-record guard on
// the quiet-path cap: a persisted record from before FirstArmedAt existed has
// the zero value there, and the cap must NOT treat that as "armed at the epoch"
// and fire immediately — it holds for the ordinary quiet window instead.
func TestQuietPathHoldsWhenFirstArmedAtIsMissing(t *testing.T) {
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	legacy := autoUpgradeDebounceState{Target: "sha-a", ArmedAt: base}

	d := shouldDebounceAutoUpgrade(legacy, "sha-a", 5*time.Minute, 10*time.Minute, base.Add(time.Minute))
	if d.Allowed {
		t.Fatalf("legacy record with zero FirstArmedAt fired early (reason %q)", d.Reason)
	}
}

// TestPersistUpgradeDebounceStateRoundTrip drives the persistence helper the
// way the trigger loop does: arm, save, re-read from disk, then clear with the
// zero state — the record a hub restart would recover must match what was
// armed, and the fire path's zero state must actually clear it.
func TestPersistUpgradeDebounceStateRoundTrip(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	s := &HubServer{logger: slog.Default()}
	armedAt := time.Date(2026, 8, 31, 12, 4, 0, 0, time.UTC)
	firstAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	h := &SaaSHive{ID: "debounce-roundtrip"}

	st := autoUpgradeDebounceState{Target: "sha-b", ArmedAt: armedAt, FirstArmedAt: firstAt, Collapsed: 1}
	s.persistUpgradeDebounceState(h, st)

	// The caller's in-memory copy must reflect the new state even before any
	// disk round-trip, so the rest of the sweep sees it.
	if h.AutoUpgradePendingTarget != "sha-b" || h.AutoUpgradeCollapsed != 1 {
		t.Fatalf("in-memory hive not updated: target %q collapsed %d", h.AutoUpgradePendingTarget, h.AutoUpgradeCollapsed)
	}

	stored := loadSaaSHive(h.ID)
	if stored == nil {
		t.Fatal("persisted hive not readable back from disk")
	}
	if stored.AutoUpgradePendingTarget != "sha-b" ||
		!stored.AutoUpgradePendingSince.Equal(armedAt) ||
		!stored.AutoUpgradePendingFirst.Equal(firstAt) ||
		stored.AutoUpgradeCollapsed != 1 {
		t.Fatalf("recovered record = {%q %v %v %d}, want the armed state back",
			stored.AutoUpgradePendingTarget, stored.AutoUpgradePendingSince,
			stored.AutoUpgradePendingFirst, stored.AutoUpgradeCollapsed)
	}

	// The fire path persists the zero state, which must CLEAR the record so a
	// consumed target cannot fire again after a restart.
	s.persistUpgradeDebounceState(h, autoUpgradeDebounceState{})
	cleared := loadSaaSHive(h.ID)
	if cleared == nil {
		t.Fatal("hive record disappeared on clear")
	}
	if cleared.AutoUpgradePendingTarget != "" || !cleared.AutoUpgradePendingSince.IsZero() ||
		!cleared.AutoUpgradePendingFirst.IsZero() || cleared.AutoUpgradeCollapsed != 0 {
		t.Fatalf("zero state did not clear the record: {%q %v %v %d}",
			cleared.AutoUpgradePendingTarget, cleared.AutoUpgradePendingSince,
			cleared.AutoUpgradePendingFirst, cleared.AutoUpgradeCollapsed)
	}
}

// TestPersistUpgradeDebounceStateNilHive pins the nil guard: the sweep can in
// principle hand the helper nothing, and that must be a no-op, not a panic.
func TestPersistUpgradeDebounceStateNilHive(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.persistUpgradeDebounceState(nil, autoUpgradeDebounceState{Target: "sha-a"})
}

// TestPersistUpgradeDebounceStateSaveFailureIsNotFatal pins the documented
// contract that a failed disk write is logged but never fatal: the in-memory
// copy is still updated, so the rest of THIS cycle behaves correctly and the
// worst case is one re-armed window after a restart.
func TestPersistUpgradeDebounceStateSaveFailureIsNotFatal(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	s := &HubServer{logger: slog.Default()}
	// A path-traversal ID is rejected by saveSaaSHive (and loadSaaSHive
	// returns nil for it), forcing both the stored==nil fallback and the
	// save-error branch.
	h := &SaaSHive{ID: "bad/id"}
	st := autoUpgradeDebounceState{Target: "sha-a", ArmedAt: time.Now(), FirstArmedAt: time.Now()}

	s.persistUpgradeDebounceState(h, st)

	if h.AutoUpgradePendingTarget != "sha-a" {
		t.Fatalf("in-memory hive not updated on save failure: target %q", h.AutoUpgradePendingTarget)
	}
}

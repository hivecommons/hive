package hub

import (
	"log/slog"
	"testing"
	"time"
)

// newBeginUpgradeServer returns a HubServer holding a single registry entry so
// the beginUpgrade / clearUpgradeLatch invariants can be exercised in isolation.
func newBeginUpgradeServer(e RegistryEntry) *HubServer {
	s := &HubServer{logger: slog.Default(), heartbeatUpgrade: make(map[string]string)}
	s.registry.Hives = []RegistryEntry{e}
	return s
}

// TestBeginUpgradeStampsFirstStart pins the base case: a hive that is NOT yet
// upgrading gets Upgrading=true, the target, and a fresh start clock.
func TestBeginUpgradeStampsFirstStart(t *testing.T) {
	s := newBeginUpgradeServer(RegistryEntry{ID: "h1"})
	before := time.Now()
	s.mu.Lock()
	s.beginUpgrade(0, "target-a")
	s.mu.Unlock()

	h := s.registry.Hives[0]
	if !h.Upgrading {
		t.Fatal("beginUpgrade must set Upgrading=true")
	}
	if h.UpgradeTarget != "target-a" {
		t.Fatalf("UpgradeTarget = %q, want target-a", h.UpgradeTarget)
	}
	if h.UpgradeStartedAt.Before(before) {
		t.Fatalf("UpgradeStartedAt must be stamped to now, got %v (before test start %v)", h.UpgradeStartedAt, before)
	}
}

// TestBeginUpgradeRetrySameTargetPreservesStart is the core of the bug fix: a
// retry that re-enters while already upgrading toward the SAME target must NOT
// overwrite UpgradeStartedAt — the elapsed clock has to keep climbing so a
// thrashing upgrade eventually crosses staleUpgradeTimeout.
func TestBeginUpgradeRetrySameTargetPreservesStart(t *testing.T) {
	orig := time.Now().Add(-53 * time.Minute) // the live epm-luiz elapsed, roughly
	s := newBeginUpgradeServer(RegistryEntry{
		ID: "h1", Upgrading: true, UpgradeTarget: "target-a", UpgradeStartedAt: orig,
	})

	s.mu.Lock()
	s.beginUpgrade(0, "target-a") // a retry toward the same target
	s.mu.Unlock()

	h := s.registry.Hives[0]
	if !h.UpgradeStartedAt.Equal(orig) {
		t.Fatalf("a same-target retry must PRESERVE UpgradeStartedAt (%v), got %v — this is the reset-every-retry bug", orig, h.UpgradeStartedAt)
	}
	if !h.Upgrading || h.UpgradeTarget != "target-a" {
		t.Fatalf("retry must keep Upgrading/target, got upgrading=%v target=%q", h.Upgrading, h.UpgradeTarget)
	}
}

// TestBeginUpgradeNewTargetRestamps: a genuinely new upgrade (the target
// changed) is a fresh upgrade and MUST get a fresh clock, or a hive that
// upgrades A -> B would show B's progress as if it had been running since A.
func TestBeginUpgradeNewTargetRestamps(t *testing.T) {
	orig := time.Now().Add(-2 * time.Hour)
	s := newBeginUpgradeServer(RegistryEntry{
		ID: "h1", Upgrading: true, UpgradeTarget: "target-a", UpgradeStartedAt: orig,
	})

	s.mu.Lock()
	s.beginUpgrade(0, "target-b") // a different target — a new upgrade
	s.mu.Unlock()

	h := s.registry.Hives[0]
	if h.UpgradeStartedAt.Equal(orig) {
		t.Fatal("a new target must re-stamp UpgradeStartedAt, but the old start survived")
	}
	if time.Since(h.UpgradeStartedAt) > time.Minute {
		t.Fatalf("a new target's clock must start now, got %v old", time.Since(h.UpgradeStartedAt))
	}
	if h.UpgradeTarget != "target-b" {
		t.Fatalf("UpgradeTarget = %q, want target-b", h.UpgradeTarget)
	}
}

// TestBeginUpgradeZeroStartSelfHeals: an upgrade latched with a LOST (zero)
// start time must get a real start on the next re-entry even if the target is
// unchanged — otherwise the ibm-alchemy wedge (Upgrading=true, zero start) stays
// unmeasurable forever.
func TestBeginUpgradeZeroStartSelfHeals(t *testing.T) {
	s := newBeginUpgradeServer(RegistryEntry{
		ID: "h1", Upgrading: true, UpgradeTarget: "target-a", // UpgradeStartedAt zero
	})

	s.mu.Lock()
	s.beginUpgrade(0, "target-a")
	s.mu.Unlock()

	h := s.registry.Hives[0]
	if h.UpgradeStartedAt.IsZero() {
		t.Fatal("a lost/zero start must be re-stamped so the upgrade becomes measurable")
	}
}

// TestClearUpgradeLatchZeroesEverything: completion/cancel/orphan-clear must
// drop the flag, the target AND the start clock so the next upgrade times from
// zero.
func TestClearUpgradeLatchZeroesEverything(t *testing.T) {
	s := newBeginUpgradeServer(RegistryEntry{
		ID: "h1", Upgrading: true, UpgradeTarget: "target-a",
		UpgradeStartedAt: time.Now().Add(-30 * time.Minute),
	})

	s.mu.Lock()
	s.clearUpgradeLatch(0)
	s.mu.Unlock()

	h := s.registry.Hives[0]
	if h.Upgrading {
		t.Error("clearUpgradeLatch must clear Upgrading")
	}
	if h.UpgradeTarget != "" {
		t.Errorf("clearUpgradeLatch must clear UpgradeTarget, got %q", h.UpgradeTarget)
	}
	if !h.UpgradeStartedAt.IsZero() {
		t.Errorf("clearUpgradeLatch must zero UpgradeStartedAt, got %v", h.UpgradeStartedAt)
	}

	// And a fresh upgrade after a clear starts a fresh clock.
	s.mu.Lock()
	s.beginUpgrade(0, "target-b")
	s.mu.Unlock()
	if time.Since(s.registry.Hives[0].UpgradeStartedAt) > time.Minute {
		t.Fatal("an upgrade begun after a clear must time from zero")
	}
}

// TestStampObservedUpgradeStampsFirstObservation: a spoke-REPORTED upgrade
// (Upgrading=true with no hub-armed clock and no previous beat to carry one
// from) must be stamped now — the dashboard badge only renders its elapsed
// counter, and the stuck-upgrade alert only fires, when the clock is non-zero.
func TestStampObservedUpgradeStampsFirstObservation(t *testing.T) {
	e := RegistryEntry{ID: "h1", Upgrading: true}
	before := time.Now()
	stampObservedUpgrade(&e, time.Time{})
	if e.UpgradeStartedAt.IsZero() {
		t.Fatal("an observed upgrade with no prior clock must be stamped")
	}
	if e.UpgradeStartedAt.Before(before) {
		t.Fatalf("first observation must stamp now, got %v (before test start %v)", e.UpgradeStartedAt, before)
	}
}

// TestStampObservedUpgradeCarriesPreviousClockForward: the heartbeat handler
// rebuilds the registry entry from scratch every beat, so a spoke-reported
// upgrade's clock lives only in the PREVIOUS entry. It must be carried
// forward, never re-stamped — a re-stamp per beat would pin the counter near
// zero for the whole upgrade (the #2725 reset bug in a new coat).
func TestStampObservedUpgradeCarriesPreviousClockForward(t *testing.T) {
	prev := time.Now().Add(-20 * time.Minute)
	e := RegistryEntry{ID: "h1", Upgrading: true}
	stampObservedUpgrade(&e, prev)
	if !e.UpgradeStartedAt.Equal(prev) {
		t.Fatalf("observed upgrade must carry the previous beat's clock forward, got %v want %v", e.UpgradeStartedAt, prev)
	}
}

// TestStampObservedUpgradeNeverTouchesALiveClock: an entry whose clock is
// already set (a hub-armed upgrade stamped by beginUpgrade) must pass through
// untouched regardless of what the previous beat carried.
func TestStampObservedUpgradeNeverTouchesALiveClock(t *testing.T) {
	orig := time.Now().Add(-45 * time.Minute)
	e := RegistryEntry{ID: "h1", Upgrading: true, UpgradeStartedAt: orig}
	stampObservedUpgrade(&e, time.Now())
	if !e.UpgradeStartedAt.Equal(orig) {
		t.Fatalf("a live clock must never be re-stamped, got %v want %v", e.UpgradeStartedAt, orig)
	}
}

// TestStampObservedUpgradeIgnoresNonUpgrading: a hive that is not upgrading
// must keep a zero clock — the invariant is bidirectional (non-zero clock
// implies an upgrade genuinely in flight; see clearUpgradeLatch).
func TestStampObservedUpgradeIgnoresNonUpgrading(t *testing.T) {
	e := RegistryEntry{ID: "h1"}
	stampObservedUpgrade(&e, time.Now().Add(-time.Hour))
	if !e.UpgradeStartedAt.IsZero() {
		t.Fatalf("a non-upgrading entry must keep a zero clock, got %v", e.UpgradeStartedAt)
	}
}

// TestObservedStampThenSameTargetBeginUpgradePreserves: an upgrade first
// stamped by the observed path, then re-entered by a hub-armed retry toward
// the SAME target, must keep the observed stamp — the two stamping paths
// share one invariant, so mixing them must not reset the clock either.
func TestObservedStampThenSameTargetBeginUpgradePreserves(t *testing.T) {
	s := newBeginUpgradeServer(RegistryEntry{ID: "h1", Upgrading: true, UpgradeTarget: "target-a"})
	stampObservedUpgrade(&s.registry.Hives[0], time.Time{})
	if s.registry.Hives[0].UpgradeStartedAt.IsZero() {
		t.Fatal("observed stamp must have set the clock")
	}
	// Backdate the observed stamp so preserve-vs-restamp is unambiguous.
	obs := time.Now().Add(-10 * time.Minute)
	s.registry.Hives[0].UpgradeStartedAt = obs

	s.mu.Lock()
	s.beginUpgrade(0, "target-a") // same-target retry
	s.mu.Unlock()

	if !s.registry.Hives[0].UpgradeStartedAt.Equal(obs) {
		t.Fatalf("a same-target beginUpgrade after an observed stamp must preserve the clock, got %v want %v", s.registry.Hives[0].UpgradeStartedAt, obs)
	}
}

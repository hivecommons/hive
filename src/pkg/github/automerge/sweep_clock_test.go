package automerge

import (
	"testing"
	"time"
)

// TestSweepClock: begin/end/armFloor shape the SweepState the dashboard
// reads (hivecommons/hive#15073); a targeted pass is on its PRs only, a
// full pass on every PR, and the snapshot is a copy.
func TestSweepClock(t *testing.T) {
	var nilClock *SweepClock
	nilClock.begin("event", nil, time.Now())
	nilClock.end(time.Now())
	nilClock.armFloor(time.Now())
	if st := nilClock.Snapshot(); st.Running || !st.NextFloorAt.IsZero() {
		t.Fatalf("nil clock must be inert: %+v", st)
	}

	c := &SweepClock{}
	t0 := time.Date(2026, 10, 28, 19, 11, 0, 0, time.UTC)
	c.armFloor(t0.Add(5 * time.Minute))
	if st := c.Snapshot(); st.Running || st.OnPR("acme/console", 1) || !st.NextFloorAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("armed, idle: %+v", st)
	}
	c.armFloorEvery(t0, 10*time.Second)
	if st := c.Snapshot(); !st.NextFloorAt.Equal(t0.Add(10*time.Second)) || st.FloorEvery != 10*time.Second {
		t.Fatalf("armed with cadence: %+v", st)
	}

	targets := SweepTargets{"acme/console": {15042: true}}
	c.begin("event", targets, t0)
	st := c.Snapshot()
	if !st.Running || st.Cause != "event" || !st.StartedAt.Equal(t0) {
		t.Fatalf("running: %+v", st)
	}
	if !st.OnPR("acme/console", 15042) || !st.OnPR("ACME/Console", 15042) || st.OnPR("acme/console", 15054) || st.OnPR("acme/other", 15042) {
		t.Errorf("targeted pass is on its PRs only: %+v", st.Targets)
	}
	st.Targets["acme/console"][15054] = true
	if c.Snapshot().OnPR("acme/console", 15054) {
		t.Errorf("Snapshot must copy targets")
	}

	c.end(t0.Add(2 * time.Second))
	if st = c.Snapshot(); st.Running || st.Targets != nil || !st.FinishedAt.Equal(t0.Add(2*time.Second)) || st.OnPR("acme/console", 15042) {
		t.Fatalf("ended: %+v", st)
	}

	c.begin("floor", nil, t0.Add(5*time.Minute))
	if st = c.Snapshot(); !st.OnPR("acme/console", 15054) || !st.OnPR("any/repo", 1) {
		t.Fatalf("full pass is on every PR: %+v", st)
	}
	c.end(t0.Add(5*time.Minute + time.Second))
}

// TestSweepClockNarrowed: once a lane marks the PR it is evaluating, a
// full pass is "on" that PR only — not every soaked row — and between PRs
// (evaluated) it is on none, until end restores the idle shape
// (hivecommons/hive#15808).
func TestSweepClockNarrowed(t *testing.T) {
	c := &SweepClock{}
	t0 := time.Date(2026, 11, 3, 11, 30, 0, 0, time.UTC)
	c.begin("floor", nil, t0)
	c.evaluating("acme/console", 15805)
	st := c.Snapshot()
	if !st.OnPR("ACME/Console", 15805) || st.OnPR("acme/console", 15791) || st.OnPR("acme/other", 15805) {
		t.Fatalf("narrowed pass is on the evaluated PR only: %+v", st)
	}
	c.evaluated()
	if st = c.Snapshot(); !st.Running || st.OnPR("acme/console", 15805) || st.OnPR("acme/console", 15791) {
		t.Fatalf("between PRs the pass is on none: %+v", st)
	}
	c.evaluating("acme/console", 15791)
	if st = c.Snapshot(); !st.OnPR("acme/console", 15791) || st.OnPR("acme/console", 15805) {
		t.Fatalf("moved on: %+v", st)
	}
	c.end(t0.Add(time.Minute))
	if st = c.Snapshot(); st.Running || st.Narrowed || st.OnPR("acme/console", 15791) {
		t.Fatalf("ended: %+v", st)
	}
	c.begin("floor", nil, t0.Add(2*time.Minute))
	if st = c.Snapshot(); !st.OnPR("acme/console", 1) {
		t.Fatalf("a fresh full pass before any mark is on every PR: %+v", st)
	}
}

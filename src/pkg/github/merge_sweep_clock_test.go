package github

import (
	"testing"
	"time"
)

func TestMergeSweepClockConcurrentLanes(t *testing.T) {
	var c MergeSweepClock
	if got := c.Snapshot("org/repo", 1); got.SweepRunning || got.LastSweepAt != nil || got.NextSweepAt != nil {
		t.Fatalf("unstarted: %+v", got)
	}
	later := time.Now().Add(5 * time.Minute)
	earlier := later.Add(-time.Minute)
	c.Schedule("lanes", later)
	c.Schedule("queued", earlier)
	endPass := c.Begin("lanes")
	endOne := c.Working("Org/Repo", 1)
	endTwo := c.Working("org/repo", 1)
	if c.Snapshot("org/repo", 2).SweepRunning {
		t.Fatal("unrelated PR appears active")
	}
	if c.Snapshot("other/repo", 1).SweepRunning {
		t.Fatal("unrelated repo appears active")
	}
	endOne()
	if !c.Snapshot("org/repo", 1).SweepRunning {
		t.Fatal("concurrent evaluation lost")
	}
	endTwo()
	endPass()
	got := c.Snapshot("org/repo", 1)
	if got.SweepRunning || got.LastSweepAt == nil || got.NextSweepAt == nil || !got.NextSweepAt.Equal(earlier) {
		t.Fatalf("completed: %+v", got)
	}
	c.Schedule("queued", time.Time{})
	if got := c.Snapshot("org/repo", 1); !got.NextSweepAt.Equal(later) {
		t.Fatalf("remaining lane: %+v", got)
	}
	var other MergeSweepClock
	if other.Snapshot("org/repo", 1).LastSweepAt != nil {
		t.Fatal("clock leaked between hives")
	}
}

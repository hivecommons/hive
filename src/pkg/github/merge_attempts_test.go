package github

import (
	"errors"
	"testing"
	"time"
)

// TestMergeAttemptsLifecycle: a merge attempt is in flight from Begin until
// End, carries the outcome (merged, or the refusal's words) after, is
// forgotten MergeAttemptRetention after it ended, and announces every
// change (hivecommons/hive#15245). A fake clock drives it.
func TestMergeAttemptsLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 31, 0, 0, time.UTC)
	m := NewMergeAttempts(func() time.Time { return now })
	changes := 0
	m.SetOnChange(func(MergeAttempt) { changes++ })

	m.Begin("HiveCommons/Hive", 15207, PRAuditPathSweep)
	a, ok := m.Get("hivecommons/hive", 15207)
	if !ok || !a.InFlight() || a.Failed() || a.Path != PRAuditPathSweep || !a.StartedAt.Equal(now) {
		t.Fatalf("after Begin: %+v ok=%v", a, ok)
	}
	if changes != 1 {
		t.Errorf("Begin must announce once, got %d", changes)
	}

	now = now.Add(2 * time.Second)
	m.End("hivecommons/hive", 15207, true, nil)
	if a, ok = m.Get("hivecommons/hive", 15207); !ok || a.InFlight() || !a.Merged || a.Failed() || !a.EndedAt.Equal(now) || a.Error != "" {
		t.Fatalf("after merged End: %+v", a)
	}
	// A second End does not rewrite the outcome.
	m.End("hivecommons/hive", 15207, false, errors.New("late"))
	if a, _ = m.Get("hivecommons/hive", 15207); !a.Merged || a.Error != "" {
		t.Errorf("second End rewrote the outcome: %+v", a)
	}
	if changes != 2 {
		t.Errorf("Begin+End announce twice, got %d", changes)
	}

	// A refusal keeps its words; an unmerged End with no error still reads
	// as a refusal.
	m.Begin("acme/console", 7, PRAuditPathDashboard)
	m.End("acme/console", 7, false, errors.New("405 Pull Request is not mergeable"))
	if a, ok = m.Get("acme/console", 7); !ok || !a.Failed() || a.Error != "405 Pull Request is not mergeable" || a.Path != PRAuditPathDashboard {
		t.Fatalf("refused: %+v", a)
	}
	m.Begin("acme/console", 8, PRAuditPathRelay)
	m.End("acme/console", 8, false, nil)
	if a, _ = m.Get("acme/console", 8); !a.Failed() || a.Error == "" {
		t.Errorf("unmerged End without error must still read as refused: %+v", a)
	}
	// End without Begin records nothing.
	m.End("acme/console", 9, true, nil)
	if _, ok = m.Get("acme/console", 9); ok {
		t.Error("End without Begin must record nothing")
	}
	if len(m.Snapshot()) != 3 {
		t.Errorf("snapshot = %d, want 3", len(m.Snapshot()))
	}

	// Finished attempts are forgotten after the retention window; one still
	// in flight is not.
	m.Begin("acme/console", 10, PRAuditPathSweep)
	now = now.Add(MergeAttemptRetention + time.Second)
	snap := m.Snapshot()
	if len(snap) != 1 || snap[0].Number != 10 || !snap[0].InFlight() {
		t.Fatalf("after retention: %+v", snap)
	}
}

package stagelabels

import (
	"testing"
	"time"
)

// NoteUnblocked keeps the lift on a stage-less record, StageSinceAt reads
// it as the since-time, and the next staging observation keeps it while
// stage entry (later) takes over (hivecommons/hive#15779).
func TestNoteUnblockedAgesRecordFromLift(t *testing.T) {
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return clock })
	key := Key("o/r", 15517)
	lifted := clock.Add(-20 * time.Minute)

	s.NoteUnblocked(key, lifted)
	rec, ok := s.Get(key)
	if !ok || rec.Stage != None || !rec.UnblockedAt.Equal(lifted) || !rec.StageSinceAt().Equal(lifted) {
		t.Fatalf("after lift: ok=%v rec=%+v since=%v, want stage-less record since the lift", ok, rec, rec.StageSinceAt())
	}

	tr := s.Observe(key, Actionable, "actionable")
	if !tr.Changed || tr.Prev != None {
		t.Fatalf("staging after lift transition = %+v, want a change from None", tr)
	}
	rec, _ = s.Get(key)
	if !rec.UnblockedAt.Equal(lifted) || !rec.StageSinceAt().Equal(clock) {
		t.Fatalf("after staging: rec=%+v since=%v, want stage entry %v", rec, rec.StageSinceAt(), clock)
	}

	// A lift after stage entry moves the since-time forward again.
	later := clock.Add(time.Hour)
	s.NoteUnblocked(key, later)
	rec, _ = s.Get(key)
	if !rec.StageSinceAt().Equal(later) {
		t.Fatalf("since after a later lift = %v, want %v", rec.StageSinceAt(), later)
	}
	s.Observe(key, None, "")
	if _, ok := s.Get(key); ok {
		t.Fatal("a None observation must drop the record, lift included")
	}
}

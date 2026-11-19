package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// hivecommons/hive#18387: the hive's one removal of `needs-triage` and its
// yield once another writer put the label back persist with the record, so
// a restart does not start the remove / re-add loop again. The yield is
// marked once (the caller logs it once) and never on a missing record.
func TestStoreTriageClearYieldSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 11, 16, 6, 1, 45, 0, time.UTC)
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), StateFile)
	s := NewStore(path, clock)
	const key = "projectbluefin/utah#1222"
	if s.MarkTriageClearYielded(key) {
		t.Fatal("no record yet: nothing to mark")
	}
	s.NoteTriageCleared(key, now)
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteTriageCleared must not invent a record")
	}
	s.Observe(key, CoveredByPR, "PR #1300 covers it")
	s.NoteTriageCleared(key, now)
	if !s.MarkTriageClearYielded(key) {
		t.Fatal("first yield must report it set the mark")
	}
	if s.MarkTriageClearYielded(key) {
		t.Fatal("a second yield must not report again (the log line is once)")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewStore(path, clock)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(key)
	if !ok || !rec.TriageClearedAt.Equal(now) || !rec.TriageClearYielded {
		t.Fatalf("reloaded record %+v: want the removal time and the yield", rec)
	}
	// A later stage move keeps both: the yield is for the life of the record.
	reloaded.Observe(key, Claimed, "claim")
	if rec, _ := reloaded.Get(key); rec.TriageClearedAt.IsZero() || !rec.TriageClearYielded {
		t.Fatalf("stage move dropped the yield: %+v", rec)
	}
}

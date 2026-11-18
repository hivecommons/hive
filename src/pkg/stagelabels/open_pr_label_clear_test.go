package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// The hive's one removal of a stale `has-open-pr` and its yield persist
// with the record; a linking PR seen beside the label again resets both so
// the label can come off once more when that PR goes.
func TestStoreOpenPRLabelClearPersistsAndResets(t *testing.T) {
	now := time.Date(2026, 11, 17, 19, 22, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), StateFile)
	s := NewStore(path, clock)
	const key = "projectbluefin/bluefin-bling#82"
	s.NoteOpenPRLabelCleared(key, now)
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteOpenPRLabelCleared must not invent a record")
	}
	if s.MarkOpenPRLabelClearYielded(key) {
		t.Fatal("no record yet: nothing to mark")
	}
	s.Observe(key, Actionable, "PR #128 closed")
	s.NoteOpenPRLabelCleared(key, now)
	if !s.MarkOpenPRLabelClearYielded(key) || s.MarkOpenPRLabelClearYielded(key) {
		t.Fatal("the yield must report set exactly once")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewStore(path, clock)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	rec, ok := reloaded.Get(key)
	if !ok || !rec.OpenPRLabelClearedAt.Equal(now) || !rec.OpenPRLabelClearYielded {
		t.Fatalf("reloaded record %+v: want the removal time and the yield", rec)
	}
	reloaded.ResetOpenPRLabelClear(key)
	if rec, _ := reloaded.Get(key); !rec.OpenPRLabelClearedAt.IsZero() || rec.OpenPRLabelClearYielded {
		t.Fatalf("reset left %+v", rec)
	}
}

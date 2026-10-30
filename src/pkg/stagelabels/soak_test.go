package stagelabels

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStoreNoteSoak: the soak clock a record carries — head, start,
// window — survives a reload, is a no-op when unchanged, is dropped by a
// blank head or zero window, and never creates a record.
func TestStoreNoteSoak(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("acme/widget", 15174)
	started := now.Add(-18*time.Minute - 400*time.Millisecond)
	s.NoteSoak(key, "abc1234def", started, 30*time.Minute)
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteSoak must not create a record")
	}
	s.Observe(key, Reviewed, "contributor-lane:claim-tier-untrusted")
	s.NoteSoak(key, " abc1234def ", started, 30*time.Minute)
	rec, _ := s.Get(key)
	head, at, window, ok := rec.Soak()
	if !ok || head != "abc1234def" || !at.Equal(started.Truncate(time.Second)) || window != 30*time.Minute {
		t.Fatalf("soak = %q %v %v ok=%v", head, at, window, ok)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.dirty = false
	s.NoteSoak(key, "abc1234def", started, 30*time.Minute)
	if s.dirty {
		t.Fatal("re-noting the same soak must not dirty the store")
	}
	reloaded := NewStore(s.path, func() time.Time { return now })
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if rec, ok := reloaded.Get(key); !ok || rec.SoakHead != "abc1234def" || rec.SoakWindowSeconds != 1800 || rec.SoakStartedAt.IsZero() {
		t.Fatalf("reloaded soak = %+v (ok=%v)", rec, ok)
	}
	// A new head restarts the clock in place.
	s.NoteSoak(key, "fff9999", now, 30*time.Minute)
	if rec, _ := s.Get(key); rec.SoakHead != "fff9999" || !rec.SoakStartedAt.Equal(now) {
		t.Fatalf("new head soak = %+v", rec)
	}
	// Blank head or zero window clears; clearing a clear record is a no-op.
	s.NoteSoak(key, "", now, 30*time.Minute)
	if rec, _ := s.Get(key); rec.SoakHead != "" || !rec.SoakStartedAt.IsZero() || rec.SoakWindowSeconds != 0 {
		t.Fatalf("blank head left %+v", rec)
	} else if _, _, _, ok := rec.Soak(); ok {
		t.Fatal("cleared record must report no soak")
	}
	s.dirty = false
	s.ClearSoak(key)
	if s.dirty {
		t.Fatal("clearing an already clear soak must not dirty the store")
	}
	s.NoteSoak(key, "abc", now, 0)
	if rec, _ := s.Get(key); rec.SoakHead != "" {
		t.Fatalf("zero window must clear, got %+v", rec)
	}
}

// TestObserveUnchangedPassDoesNotDirty (hivecommons/hive#15729): a pass
// that re-observes the same stage and reason moves LastSeen in memory but
// leaves the saved file alone until LastSeenPersistEvery has passed; any
// other change still dirties it, and a reason of a new class stamps
// ReasonChangedAt.
func TestObserveUnchangedPassDoesNotDirty(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := Key("acme/widget", 15730)
	s.Observe(key, PreMergeSoak, "contributor-lane:soaking")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	entered := now
	for i := 1; i <= 5; i++ {
		now = entered.Add(time.Duration(i) * time.Minute)
		if tr := s.Observe(key, PreMergeSoak, "contributor-lane:soaking"); tr.Changed {
			t.Fatalf("pass %d: unchanged observation reported a change", i)
		}
		if s.Dirty() {
			t.Fatalf("pass %d: an unchanged pass dirtied the store", i)
		}
		rec, _ := s.Get(key)
		if !rec.LastSeen.Equal(now) {
			t.Fatalf("pass %d: LastSeen = %v, want %v (the live store keeps the exact sweep time)", i, rec.LastSeen, now)
		}
		if !rec.ReasonChangedAt.Equal(entered) || !rec.NoteChangedAt().Equal(entered) {
			t.Fatalf("pass %d: note changed at %v, want stage entry %v", i, rec.ReasonChangedAt, entered)
		}
	}
	// Once the saved LastSeen is LastSeenPersistEvery old the pass writes it.
	now = entered.Add(LastSeenPersistEvery)
	s.Observe(key, PreMergeSoak, "contributor-lane:soaking")
	if !s.Dirty() {
		t.Fatal("a LastSeen older than LastSeenPersistEvery on disk must be persisted")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s.Observe(key, PreMergeSoak, "contributor-lane:soaking")
	if s.Dirty() {
		t.Fatal("the pass after a save is unchanged again")
	}
	// A reason of a new class at the same stage is a change: the note moved.
	now = now.Add(time.Minute)
	if tr := s.Observe(key, PreMergeSoak, "release-hold"); tr.Changed {
		t.Fatal("a reason change is not a stage change")
	}
	if !s.Dirty() {
		t.Fatal("a new reason must dirty the store")
	}
	if rec, _ := s.Get(key); !rec.ReasonChangedAt.Equal(now) || !rec.NoteChangedAt().Equal(now) {
		t.Fatalf("ReasonChangedAt = %v, want %v", rec.ReasonChangedAt, now)
	}
	// NoteActivity with nothing new leaves a clean store clean.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	a := Activity{Head: "abc", Comments: 2, Reviews: 1, Digest: "d"}
	s.NoteActivity(key, a)
	if !s.Dirty() {
		t.Fatal("the first activity observation must be written")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.NoteActivity(key, a)
	if s.Dirty() {
		t.Fatal("re-observing the same activity must not dirty the store")
	}
}

// TestReasonClassAndLegacySoakRecord: the pre-#15729 soak reason carried
// its countdown (`soaking(remaining=12m0s)`), so two passes differ only in
// the detail; ReasonClass reads them as one note, and a record written
// that way loads and does not stamp ReasonChangedAt when the bare reason
// replaces it.
func TestReasonClassAndLegacySoakRecord(t *testing.T) {
	for in, want := range map[string]string{
		"contributor-lane:soaking(remaining=12m0s)": "contributor-lane:soaking",
		"contributor-lane:soaking":                  "contributor-lane:soaking",
		" check-failing:unit ":                      "check-failing:unit",
		"":                                          "",
	} {
		if got := ReasonClass(in); got != want {
			t.Errorf("ReasonClass(%q) = %q, want %q", in, got, want)
		}
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "stage.json")
	legacy := `{"acme/widget#15730":{"stage":` + fmt.Sprint(int(PreMergeSoak)) + `,"entered_at":"2026-10-30T11:40:00Z","reason":"contributor-lane:soaking(remaining=12m0s)","last_seen":"2026-10-30T11:58:00Z"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path, func() time.Time { return now })
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	key := Key("acme/widget", 15730)
	rec, ok := s.Get(key)
	if !ok || rec.Stage != PreMergeSoak || !rec.ReasonChangedAt.IsZero() || !rec.NoteChangedAt().Equal(rec.EnteredAt) {
		t.Fatalf("legacy record = %+v ok=%v", rec, ok)
	}
	if _, _, _, soaking := rec.Soak(); soaking {
		t.Fatal("a legacy record carries no soak clock of its own")
	}
	// The first pass of the new code writes the bare reason: same class,
	// so the note did not change; the store is dirty for the reason text.
	s.Observe(key, PreMergeSoak, "contributor-lane:soaking")
	rec, _ = s.Get(key)
	if rec.Reason != "contributor-lane:soaking" || !rec.ReasonChangedAt.IsZero() {
		t.Fatalf("after upgrade: %+v", rec)
	}
	if !s.Dirty() {
		t.Fatal("the reason text changed; the store must be written once")
	}
}

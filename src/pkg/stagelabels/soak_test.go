package stagelabels

import (
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

package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// The owed branch-update clock of an agent's fork-workflow-update head: the
// first note for a head keeps its time, a new head restarts it, a clear
// drops it, and a read for another head reads nothing.
func TestStoreAuthorUpdateOwed(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "stages.json"), func() time.Time { return t0 })
	s.NoteAuthorUpdateOwed("missing", "aaa", t0) // unknown key: no-op
	if _, ok := s.Get("missing"); ok {
		t.Fatal("NoteAuthorUpdateOwed created a record")
	}
	s.ClearAuthorUpdateOwed("missing") // unknown key: no-op

	key := Key("acme/widget", 7)
	s.Observe(key, ReturnedToContributor, "fork-workflow-update")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.ClearAuthorUpdateOwed(key) // nothing to clear: stays clean
	if s.Dirty() {
		t.Fatal("clearing an empty clock marked the store dirty")
	}
	s.NoteAuthorUpdateOwed(key, "", t0) // no head: no-op
	if r, _ := s.Get(key); !r.AuthorUpdateOwedSince("").IsZero() || r.AuthorUpdateOwedHead != "" {
		t.Fatalf("an empty head was noted: %+v", r)
	}

	s.NoteAuthorUpdateOwed(key, "aaa", t0)
	r, _ := s.Get(key)
	if got := r.AuthorUpdateOwedSince("aaa"); !got.Equal(t0) {
		t.Fatalf("owed since = %v, want %v", got, t0)
	}
	if got := r.AuthorUpdateOwedSince("bbb"); !got.IsZero() {
		t.Fatalf("another head reads %v, want zero", got)
	}
	if !s.Dirty() {
		t.Fatal("the first note did not mark the store dirty")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s.NoteAuthorUpdateOwed(key, "aaa", t0.Add(time.Hour)) // same head: keeps the first time
	if r, _ := s.Get(key); !r.AuthorUpdateOwedSince("aaa").Equal(t0) {
		t.Fatalf("a repeat note moved the clock: %+v", r)
	}
	if s.Dirty() {
		t.Fatal("a repeat note marked the store dirty")
	}

	t1 := t0.Add(2 * time.Hour)
	s.NoteAuthorUpdateOwed(key, "bbb", t1) // new head: fresh clock
	if r, _ := s.Get(key); !r.AuthorUpdateOwedSince("bbb").Equal(t1) || !r.AuthorUpdateOwedSince("aaa").IsZero() {
		t.Fatalf("a new head did not restart the clock: %+v", r)
	}

	s.ClearAuthorUpdateOwed(key)
	if r, _ := s.Get(key); r.AuthorUpdateOwedHead != "" || !r.AuthorUpdateOwedAt.IsZero() || !r.AuthorUpdateOwedSince("bbb").IsZero() {
		t.Fatalf("clear left the clock: %+v", r)
	}
	if !s.Dirty() {
		t.Fatal("a clear did not mark the store dirty")
	}
}

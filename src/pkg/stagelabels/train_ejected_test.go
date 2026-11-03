package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreTrainEjectedPersistsAndClears(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "stage.json")
	s := NewStore(path, func() time.Time { return now })
	key := Key("acme/widget", 15825)
	s.Observe(key, MergeQueue, "queued")
	r, _ := s.Get(key)
	if why, head, at, ok := r.TrainEjected(); ok || why != "" || head != "" || !at.IsZero() {
		t.Fatalf("new record reports an ejection: %q %q %v %v", why, head, at, ok)
	}

	ejectedAt := now.Add(-time.Minute)
	s.NoteTrainEjected(key, " aaa1111 ", " required-check-failing ", ejectedAt)
	r, _ = s.Get(key)
	if why, head, at, ok := r.TrainEjected(); !ok || why != "required-check-failing" || head != "aaa1111" || !at.Equal(ejectedAt) || !s.dirty {
		t.Fatalf("ejection not recorded: %+v dirty=%v", r, s.dirty)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s = NewStore(path, func() time.Time { return now })
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(key)
	if why, head, at, ok := r.TrainEjected(); !ok || why != "required-check-failing" || head != "aaa1111" || !at.Equal(ejectedAt) {
		t.Fatalf("ejection lost after reload: %+v", r)
	}

	s.dirty = false
	s.NoteTrainEjected(key, "aaa1111", "required-check-failing", now)
	r, _ = s.Get(key)
	if !r.TrainEjectedAt.Equal(ejectedAt) || s.dirty {
		t.Fatalf("same ejection must keep its original instant: %+v dirty=%v", r, s.dirty)
	}

	s.NoteTrainEjected(key, "bbb2222", "base-moved", now)
	r, _ = s.Get(key)
	if why, head, at, ok := r.TrainEjected(); !ok || why != "base-moved" || head != "bbb2222" || !at.Equal(now) || !s.dirty {
		t.Fatalf("new head/cause must replace the ejection: %+v dirty=%v", r, s.dirty)
	}

	s.dirty = false
	s.NoteTrainEjected(key, "bbb2222", " \t ", now)
	r, _ = s.Get(key)
	if _, _, _, ok := r.TrainEjected(); ok || r.TrainEjectedWhy != "" || r.TrainEjectedHead != "" || !r.TrainEjectedAt.IsZero() || !s.dirty {
		t.Fatalf("empty reason must clear every ejection field: %+v dirty=%v", r, s.dirty)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s = NewStore(path, func() time.Time { return now })
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(key)
	if _, _, _, ok := r.TrainEjected(); ok || r.TrainEjectedWhy != "" || r.TrainEjectedHead != "" || !r.TrainEjectedAt.IsZero() {
		t.Fatalf("cleared ejection reappeared after reload: %+v", r)
	}
	s.dirty = false
	s.ClearTrainEjected(key)
	if s.dirty {
		t.Fatal("clearing an already-clear record must be a no-op")
	}
}

func TestStoreTrainEjectedClockAndMissingRecord(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		at   time.Time
	}{
		{name: "zero"},
		{name: "future", at: now.Add(time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
			key := Key("acme/widget", 15825)
			s.NoteTrainEjected(key, "aaa1111", "check-failing", tt.at)
			s.ClearTrainEjected(key)
			if _, ok := s.Get(key); ok || s.dirty {
				t.Fatal("ejection updates must not create missing records or dirty the store")
			}
			s.Observe(key, MergeQueue, "queued")
			s.NoteTrainEjected(key, "aaa1111", "check-failing", tt.at)
			r, _ := s.Get(key)
			if why, head, at, ok := r.TrainEjected(); !ok || why != "check-failing" || head != "aaa1111" || !at.Equal(now) {
				t.Fatalf("invalid ejection instant must clamp to the store clock: %+v", r)
			}
		})
	}
}

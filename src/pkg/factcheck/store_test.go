package factcheck

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTripAndRetention(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	if got := s.Get("o/r#1"); got != (State{}) {
		t.Fatalf("unseen PR must be zero, got %+v", got)
	}
	if err := s.Update("o/r#1", t0, func(st *State) { st.ReviewKicks = 2; st.LastReviewHead = headOne }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.Update("o/r#2", t0, func(st *State) { st.NeedsHumanNoted = true }); err != nil {
		t.Fatalf("update: %v", err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := again.Get("o/r#1"); got.ReviewKicks != 2 || got.LastReviewHead != headOne {
		t.Fatalf("state not persisted: %+v", got)
	}
	if !again.Get("o/r#2").NeedsHumanNoted {
		t.Fatal("hand-off flag not persisted")
	}

	// A much later write prunes records nobody touched for the retention window.
	if err := again.Update("o/r#3", t0.Add(stateRetention+time.Hour), func(st *State) { st.ReviewKicks = 1 }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := again.Get("o/r#1"); got != (State{}) {
		t.Fatalf("stale record should be pruned, got %+v", got)
	}
	if again.Get("o/r#3").ReviewKicks != 1 {
		t.Fatal("fresh record kept")
	}
}

func TestStoreCorruptFileStillUsable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), filePerm); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err == nil {
		t.Fatal("a corrupt file is reported")
	}
	if uerr := s.Update("o/r#1", t0, func(st *State) { st.ReviewKicks = 1 }); uerr != nil {
		t.Fatalf("a corrupt file is overwritten on the next save: %v", uerr)
	}
	if s.Get("o/r#1").ReviewKicks != 1 {
		t.Fatal("state kept in memory")
	}
}

func TestStoreUnwritableDirKeepsMemory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), filePerm); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(filepath.Join(blocker, "sub"))
	if err := s.Update("o/r#1", t0, func(st *State) { st.ReviewKicks = 1 }); err == nil {
		t.Fatal("an unwritable directory is reported")
	}
	if s.Get("o/r#1").ReviewKicks != 1 {
		t.Fatal("the round is still counted in memory")
	}
}

func TestNilStoreIsInert(t *testing.T) {
	var s *Store
	if got := s.Get("k"); got != (State{}) {
		t.Fatalf("nil store Get = %+v", got)
	}
	if err := s.Update("k", t0, func(*State) {}); err != nil {
		t.Fatalf("nil store Update = %v", err)
	}
}

func TestDirEnvOverride(t *testing.T) {
	t.Setenv(DirEnvVar, "")
	if Dir() != DefaultDir {
		t.Fatalf("default dir = %q", Dir())
	}
	t.Setenv(DirEnvVar, "/tmp/elsewhere")
	if Dir() != "/tmp/elsewhere" {
		t.Fatalf("override dir = %q", Dir())
	}
}

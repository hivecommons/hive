package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- LastSeen ----

func TestLastSeen_NilReceiver(t *testing.T) {
	var b *Bead
	got, ok := b.LastSeen()
	if ok {
		t.Fatalf("nil bead: ok = true, want false")
	}
	if !got.IsZero() {
		t.Fatalf("nil bead: got %v, want zero time", got)
	}
}

func TestLastSeen_NeverStamped(t *testing.T) {
	b := &Bead{}
	got, ok := b.LastSeen()
	if ok {
		t.Fatalf("unstamped bead: ok = true, want false")
	}
	if !got.IsZero() {
		t.Fatalf("unstamped bead: got %v, want zero time", got)
	}
}

func TestLastSeen_Stamped(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b := &Bead{LastSeenAt: &flexTime{want}}
	got, ok := b.LastSeen()
	if !ok {
		t.Fatalf("stamped bead: ok = false, want true")
	}
	if !got.Equal(want) {
		t.Fatalf("stamped bead: got %v, want %v", got, want)
	}
}

// ---- appendArchiveEntry ----

func TestAppendArchiveEntry_NilBead(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.appendArchiveEntry(nil) {
		t.Fatalf("appendArchiveEntry(nil) = true, want false")
	}
}

func TestAppendArchiveEntry_MarshalError(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// A channel in Metadata is not JSON-encodable, so the archive record cannot
	// be built and the entry must report "not on disk".
	b := &Bead{ID: "b-marshal", Title: "t", Status: StatusClosed,
		Metadata: map[string]interface{}{"bad": make(chan int)}}
	if s.appendArchiveEntry(b) {
		t.Fatalf("appendArchiveEntry with unmarshalable metadata = true, want false")
	}
}

func TestAppendArchiveEntry_OpenError(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// A directory squatting on the archive path makes O_APPEND|O_CREATE|O_WRONLY
	// fail, which must report "not on disk" so the caller keeps the bead.
	if err := os.Mkdir(filepath.Join(dir, archiveFileName), 0o755); err != nil {
		t.Fatalf("mkdir archive path: %v", err)
	}
	b := &Bead{ID: "b1", Title: "t", Status: StatusClosed}
	if s.appendArchiveEntry(b) {
		t.Fatalf("appendArchiveEntry with unopenable archive = true, want false")
	}
}

// ---- persist ----

func TestPersist_CreateTempError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not read-only for root")
	}
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o770) })

	err = s.persist(nil)
	if err == nil {
		t.Fatalf("persist into read-only dir: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "creating tmp beads file") {
		t.Fatalf("persist error = %q, want it to mention creating tmp beads file", err)
	}
}

func TestPersist_RenameError(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// A non-empty directory at the beads.json path defeats the final rename:
	// the temp write succeeds but the atomic swap cannot land.
	target := filepath.Join(dir, beadsFileName)
	_ = os.Remove(target)
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}

	if err := s.persist(nil); err == nil {
		t.Fatalf("persist over non-empty dir: err = nil, want rename error")
	}

	// The failed persist must not leave its unique temp file behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file after failed persist: %s", e.Name())
		}
	}
}

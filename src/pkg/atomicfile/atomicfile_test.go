package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func noTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file %q left behind", e.Name())
		}
	}
}

func TestWriteFileReplacesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600 (the old inode's 644 must not carry over)", perm)
	}
	noTemps(t, dir)
}

// A stale path+".tmp" left by an older fixed-name writer (or planted by
// anything else) must not block or be clobbered by a write: the temp name is
// unique.
func TestWriteFileIgnoresFixedTmpSibling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile with a stale %s.tmp directory: %v", path, err)
	}
	if info, err := os.Stat(path + ".tmp"); err != nil || !info.IsDir() {
		t.Fatalf("stale .tmp sibling was touched: %v", err)
	}
}

// Concurrent writers to one path must all succeed and leave one complete
// payload: with a shared fixed temp name one writer renames another's file
// away and fails with ENOENT (#16629).
func TestWriteFileConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	const writers = 16
	payloads := make(map[string]bool, writers)
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		p := fmt.Sprintf(`{"writer":%d,"pad":%q}`, i, strings.Repeat("x", 4096))
		payloads[p] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- WriteFile(path, []byte(p), 0o644)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteFile: %v", err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !payloads[string(got)] {
		t.Fatalf("final content is not one writer's complete payload (len %d)", len(got))
	}
	noTemps(t, dir)
}

// A rename that cannot land (here: onto a non-empty directory) leaves the
// target alone, removes the temp file, and surfaces an *os.LinkError.
func TestWriteFileRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(path, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := WriteFile(path, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("rename onto a non-empty directory succeeded")
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("err = %v (%T), want a wrapped *os.LinkError", err, err)
	}
	noTemps(t, dir)
}

func TestWriteFileMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := WriteFile(path, []byte("x"), 0o644); err == nil {
		t.Fatal("WriteFile into a missing directory succeeded")
	}
}

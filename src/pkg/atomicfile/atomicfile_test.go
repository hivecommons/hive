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

// faultyFile wraps a real temp file and fails the named step, so the cleanup
// of the on-disk temp file is exercised for real.
type faultyFile struct {
	*os.File
	failOn string
}

var errInjected = errors.New("injected failure")

func (f *faultyFile) Chmod(m os.FileMode) error {
	if f.failOn == "chmod" {
		return errInjected
	}
	return f.File.Chmod(m)
}

func (f *faultyFile) Write(b []byte) (int, error) {
	if f.failOn == "write" {
		return 0, errInjected
	}
	return f.File.Write(b)
}

func (f *faultyFile) Sync() error {
	if f.failOn == "sync" {
		return errInjected
	}
	return f.File.Sync()
}

func (f *faultyFile) Close() error {
	if f.failOn == "close" {
		_ = f.File.Close()
		return errInjected
	}
	return f.File.Close()
}

func injectFailure(t *testing.T, step string) {
	t.Helper()
	orig := createTemp
	t.Cleanup(func() { createTemp = orig })
	createTemp = func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &faultyFile{File: f, failOn: step}, nil
	}
}

// A failure at any step after the temp file exists removes the temp file,
// leaves the target untouched, and names the failing step.
func TestWriteFileStepFailures(t *testing.T) {
	for _, step := range []string{"chmod", "write", "sync", "close"} {
		t.Run(step, func(t *testing.T) {
			injectFailure(t, step)
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := WriteFile(path, []byte("new"), 0o600)
			if !errors.Is(err, errInjected) {
				t.Fatalf("err = %v, want the injected %s failure", err, step)
			}
			if !strings.Contains(err.Error(), "temp file") {
				t.Fatalf("err = %v, want it to name the temp file", err)
			}
			got, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(got) != "old" {
				t.Fatalf("target = %q after failed %s, want %q", got, step, "old")
			}
			noTemps(t, dir)
		})
	}
}

// A missing parent directory fails at temp creation, names the directory,
// and creates nothing.
func TestWriteFileMissingDirError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "missing")
	err := WriteFile(filepath.Join(dir, "state.json"), []byte("x"), 0o644)
	if err == nil {
		t.Fatal("WriteFile into a missing directory succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want a wrapped os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v, want it to name %s", err, dir)
	}
	if _, serr := os.Stat(dir); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("missing directory was created: %v", serr)
	}
}

// An unwritable parent directory fails at temp creation and leaves the
// existing target alone.
func TestWriteFileUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := WriteFile(path, []byte("new"), 0o644); err == nil {
		t.Fatal("WriteFile into a read-only directory succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("target = %q, want %q", got, "old")
	}
}

// WriteFile creates a target that did not exist, with the requested mode
// applied exactly even when it has bits the umask would normally strip.
func TestWriteFileCreatesNewFileWithExactMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o666} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fresh.json")
			if err := WriteFile(path, []byte("data"), mode); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != mode {
				t.Fatalf("mode = %o, want %o", perm, mode)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "data" {
				t.Fatalf("content = %q, want %q", got, "data")
			}
			noTemps(t, dir)
		})
	}
}

// SyncDir is best effort: a directory that cannot be opened is ignored, and
// a real directory syncs without panicking.
func TestSyncDir(t *testing.T) {
	SyncDir(filepath.Join(t.TempDir(), "does-not-exist"))
	SyncDir(t.TempDir())
}

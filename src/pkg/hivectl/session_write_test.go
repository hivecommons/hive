package hivectl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests exercise SessionStore.write directly. The Save/Delete failure
// tests in session_test.go mostly fail inside read() before write() ever runs
// (a directory at the cache path breaks ReadFile first), so write()'s own
// error branches — MkdirAll, CreateTemp, and Rename — were unpinned. write()
// is the atomic-replace seam that keeps a credential cache from being torn
// mid-update; each branch below is a distinct way that replace can fail.

// TestSessionWriteMkdirAllFails pins the first branch: a FILE occupying the
// config-dir path makes MkdirAll fail, and the error must name the directory
// it could not create so the operator knows what is in the way.
func TestSessionWriteMkdirAllFails(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "hive")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(filepath.Join(blocker, "sessions.json"))

	err := store.write(&sessionFile{})
	if err == nil {
		t.Fatal("write() = nil with a file blocking the config dir, want an error")
	}
	if !strings.Contains(err.Error(), "create hive config dir") {
		t.Errorf("write() error = %q, want it to name the config dir failure", err)
	}
	if !strings.Contains(err.Error(), blocker) {
		t.Errorf("write() error = %q, want it to include the blocked path %q", err, blocker)
	}
}

// TestSessionWriteCreateTempFails pins the CreateTemp branch: the config dir
// exists but is not writable, so MkdirAll succeeds (the dir is already there)
// and the failure surfaces one step later, when the temp file cannot be
// created. This is the branch the Save-based test could not isolate.
func TestSessionWriteCreateTempFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory write bits are not meaningful on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "hive")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	store := NewSessionStore(filepath.Join(dir, "sessions.json"))

	err := store.write(&sessionFile{})
	if err == nil {
		t.Fatal("write() = nil with an unwritable config dir, want an error")
	}
	if !strings.Contains(err.Error(), "write hive session cache") {
		t.Errorf("write() error = %q, want the write-cache wrapper", err)
	}
}

// TestSessionWriteRenameFails pins the last branch: everything up to the
// atomic replace works, then Rename fails because a DIRECTORY sits at the
// destination. The contract on failure is no litter — the temp file must be
// removed, not abandoned next to the cache.
func TestSessionWriteRenameFails(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "sessions.json")
	// A non-empty directory at the target defeats rename on every platform
	// (an empty one can be replaced by rename(2) on some systems).
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(target)

	err := store.write(&sessionFile{})
	if err == nil {
		t.Fatal("write() = nil renaming onto a non-empty directory, want an error")
	}
	if !strings.Contains(err.Error(), "write hive session cache") {
		t.Errorf("write() error = %q, want the write-cache wrapper", err)
	}

	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sessions-") && strings.HasSuffix(e.Name(), ".json.tmp") {
			t.Errorf("write() left temp file %q behind after a failed rename", e.Name())
		}
	}
}

// TestSessionWriteFailureLeavesOldCacheIntact is the reason write() bothers
// with temp-and-rename at all: a failed update must not tear the cache that
// was already there. A blocked rename may not be reachable once a valid cache
// exists (read() succeeds, rename onto a file succeeds), so this drives the
// failure through an unwritable directory and asserts the original bytes
// survive untouched.
func TestSessionWriteFailureLeavesOldCacheIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory write bits are not meaningful on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "hive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sessions.json")
	store := NewSessionStore(path)
	if err := store.Save("http://127.0.0.1:3001", testSession("hive_session=keep-me")); err != nil {
		t.Fatalf("seed Save() = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := store.Save("http://127.0.0.1:3001", testSession("hive_session=clobber")); err == nil {
		t.Fatal("Save() = nil into an unwritable dir, want an error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cache unreadable after failed Save: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("failed Save mutated the existing cache:\nbefore: %s\nafter:  %s", before, after)
	}
	sess, err := store.Load("http://127.0.0.1:3001")
	if err != nil {
		t.Fatalf("Load() after failed Save = %v", err)
	}
	if sess == nil || sess.Cookie != "hive_session=keep-me" {
		t.Fatalf("Load() = %+v, want the original hive_session=keep-me session", sess)
	}
}

// TestSessionWriteSuccessIsAtomicReplace pins the happy-path contract from
// the caller's side: writing over an existing cache replaces it in one step,
// keeps the 0600 mode, and leaves no temp files behind.
func TestSessionWriteSuccessIsAtomicReplace(t *testing.T) {
	store := tempStore(t)
	const server = "http://127.0.0.1:3001"
	if err := store.Save(server, testSession("hive_session=first")); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if err := store.Save(server, testSession("hive_session=second")); err != nil {
		t.Fatalf("second Save() = %v", err)
	}

	sess, err := store.Load(server)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if sess == nil || sess.Cookie != "hive_session=second" {
		t.Fatalf("Load() = %+v, want the replacing hive_session=second session", sess)
	}

	dir := filepath.Dir(store.Path())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("successful write left temp file %q behind", e.Name())
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.Path())
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != sessionFileMode {
			t.Errorf("rewritten cache mode = %o, want %o", perm, sessionFileMode)
		}
	}
}

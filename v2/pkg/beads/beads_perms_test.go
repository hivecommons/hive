package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// TestNewStore_CreatesGroupWritableDir verifies the permission fix: NewStore
// creates its directory (so a missing /data/beads/<agent> is provisioned) and
// requests group-writable (0770) perms — the dashboard/hub process mints an
// issue-sourced epic into the architect's store as a different UID in the shared
// node group, so the dir must be writable by the group, not owner-only.
//
// NewStore explicitly chmods after creation so the result is not clipped by
// umask. It must also keep the directory private to owner+node group; world
// read/traverse would weaken the per-UID agent isolation boundary.
func TestNewSharedStore_CreatesGroupWritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "beads", "architect")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: dir should not exist yet, stat err=%v", err)
	}

	s, err := NewSharedStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected a directory at %s", dir)
	}
	if mode := fi.Mode().Perm(); mode != 0o770 {
		t.Errorf("dir mode = %o, want 770", mode)
	}
	// Deliberately NOT guarded on runtime.GOOS: setgid is set through the
	// portable os.ModeSetgid, which macOS honours too. The old linux-only guard
	// is exactly why the underlying bug survived — os.Chmod(dir, 0o2770) drops
	// setgid before the syscall (os.FileMode expects os.ModeSetgid, not the Unix
	// octal 0o2000), so the dir came out plain 0770 everywhere, but only Linux
	// CI ever asserted it. Keeping this cross-platform means a dev machine
	// catches the next regression instead of shipping it to CI.
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("dir mode %v lacks setgid bit", fi.Mode())
	}

	// Minting a bead must persist a beads.json — proving the store is writable and
	// exercising the persist path whose tmp file is created 0660 (group-writable).
	if _, err := s.Create("epic", TypeEpic, PriorityHigh, "architect", "gh-a/b#1"); err != nil {
		t.Fatalf("Create (persist): %v", err)
	}
	fpath := filepath.Join(dir, beadsFileName)
	ffi, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("beads file not persisted: %v", err)
	}
	fmode := ffi.Mode().Perm()
	if fmode&0200 == 0 {
		t.Errorf("beads file mode %o lacks owner-write bit", fmode)
	}
	if umaskAllowsGroupWrite(t) && fmode&0020 == 0 {
		t.Errorf("beads file mode %o lacks group-write bit (0660 regression?)", fmode)
	}
}

// umaskAllowsGroupWrite reports whether the process umask leaves the group-write
// bit (0020) unmasked, so a group-writable create can actually land it. It reads
// and restores the umask non-destructively.
func umaskAllowsGroupWrite(t *testing.T) bool {
	t.Helper()
	// syscall.Umask both sets and returns the previous value; call twice to read
	// without changing it.
	old := syscall.Umask(0)
	syscall.Umask(old)
	return old&0020 == 0
}

// TestNewStore_CreatesPrivateDir pins the other half of the split: the
// default constructor is owner-private (bd scratch stores, Visual Hive
// state dirs); group-shared role stores must opt in via NewSharedStore.
func TestNewStore_CreatesPrivateDir(t *testing.T) {
	dir := t.TempDir() + "/private-store"
	if _, err := NewStore(dir); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("dir not created: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 700", mode)
	}
	if fi.Mode()&os.ModeSetgid != 0 {
		t.Errorf("private store must not carry setgid, got %v", fi.Mode())
	}
}

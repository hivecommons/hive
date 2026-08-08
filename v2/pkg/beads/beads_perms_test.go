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
func TestNewStore_CreatesGroupWritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "beads", "architect")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: dir should not exist yet, stat err=%v", err)
	}

	s, err := NewStore(dir)
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
	if runtime.GOOS == "linux" && fi.Mode()&os.ModeSetgid == 0 {
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

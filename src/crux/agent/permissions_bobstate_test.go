package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bobStateStartMode is the exact production-observed starting mode for a broken
// bob state dir: owner rwx, group r-x, other r-x (0755). The group lacks write,
// so bob — which reaches the dir as an agent UID through the node group — cannot
// persist settings or initialize its logger and runs degraded.
const bobStateStartMode os.FileMode = 0o755

// bobStateWantMode is the mode after remediation: the group must have gained
// r/w/x, bringing the dir to at least 0770. Owner and other bits are preserved.
const bobStateWantMode os.FileMode = 0o775

// mkBobStateDir creates a `.bob` directory pinned to a given mode (Mkdir is
// umask-filtered, so the mode is re-applied explicitly).
func mkBobStateDir(t *testing.T, parent string, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(parent, bobStateDirBase)
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	return dir
}

// TestFixBobStateDirGroupWriteAddsGroupBits is the core fail-before/pass-after:
// a .bob dir created 0755 (no group write) must come back group-writable so an
// agent UID in the node group can persist bob's state.
func TestFixBobStateDirGroupWriteAddsGroupBits(t *testing.T) {
	dir := mkBobStateDir(t, t.TempDir(), bobStateStartMode)

	// Precondition: the group genuinely lacks write before remediation, so the
	// assertion below is meaningful (fail-before).
	if before := statMode(t, dir); before&bobStateDirGroupRWX == bobStateDirGroupRWX {
		t.Fatalf("precondition: %s already group-rwx at %v; test asserts nothing", dir, before)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fixBobStateDirGroupWrite(dir, fi.Mode(), quietLogger())

	got := statMode(t, dir)
	if got&bobStateDirGroupRWX != bobStateDirGroupRWX {
		t.Errorf("mode = %v, want group r/w/x (%v) set so an agent UID can write", got, bobStateDirGroupRWX)
	}
	if got != bobStateWantMode {
		t.Errorf("mode = %v, want exactly %v (only group bits added, owner/other preserved)", got, bobStateWantMode)
	}
	if got&bobStateGroupWriteBit == 0 {
		t.Errorf("mode = %v, want the group write bit that lets bob save settings", got)
	}
}

// TestFixBobStateDirGroupWriteIsIdempotent pins that an already-group-writable
// dir is left byte-identical: the watcher must not churn a healthy dir every
// tick, and must never narrow a dir that has MORE than the required bits.
func TestFixBobStateDirGroupWriteIsIdempotent(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o775, 0o777} {
		dir := mkBobStateDir(t, t.TempDir(), mode)

		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		fixBobStateDirGroupWrite(dir, fi.Mode(), quietLogger())

		if got := statMode(t, dir); got != mode {
			t.Errorf("mode = %v, want it unchanged at %v: an already-group-writable dir must not be touched", got, mode)
		}
	}
}

// TestFixBobStateDirGroupWriteHandlesChmodFailure covers the EPERM arm: when the
// chmod cannot be performed the function must log and return, never panic. We
// force the failure by pointing at a path inside a read-only parent (the child
// dir's mode cannot be changed once its parent denies write to a non-owner path
// on most systems); more portably, we remove the dir out from under the call so
// chmod hits ENOENT. Either way the contract is: no panic, mode untouched.
func TestFixBobStateDirGroupWriteHandlesChmodFailure(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, bobStateDirBase)

	// Capture a FileInfo for a dir that no longer exists, so the chmod inside
	// the function fails with ENOENT — exercising the error path without
	// requiring elevated privileges to manufacture an EPERM.
	if err := os.Mkdir(dir, bobStateStartMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Must not panic; must return cleanly after logging.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fixBobStateDirGroupWrite panicked on chmod failure: %v", r)
		}
	}()
	fixBobStateDirGroupWrite(dir, fi.Mode(), quietLogger())

	// The dir stays gone — the function did not recreate or otherwise mutate.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("expected %s to remain absent after a failed chmod, stat err = %v", dir, statErr)
	}
}

// TestFixEntryRemediatesBobStateDir proves the wiring: fixEntry, the per-entry
// callback the walk invokes, recognizes a .bob directory by name and widens it
// even though the general owner guard would otherwise skip a non-DevUID dir.
// The test process owns the temp dir, so the chmod is permitted here.
func TestFixEntryRemediatesBobStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := mkBobStateDir(t, t.TempDir(), bobStateStartMode)

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fixEntry(dir, fi, quietLogger())

	if got := statMode(t, dir); got&bobStateDirGroupRWX != bobStateDirGroupRWX {
		t.Errorf("mode = %v, want fixEntry to have added group r/w/x (%v) to the .bob dir", got, bobStateDirGroupRWX)
	}
}

// TestFixEntryIgnoresNonBobDirs guards scope: a directory that is NOT a .bob
// state dir must not be widened by the bob-specific arm. (Owner-guarded general
// widening may still apply when the process owns the dir, so we assert only that
// a non-.bob dir owned by the test process is not force-group-widened past what
// the general DirPerms arm would do — here we use a name that is not ".bob" and
// a mode already satisfying DirPerms so neither arm should touch it.)
func TestFixEntryIgnoresNonBobDirs(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "not-dot-bob")
	if err := os.Mkdir(other, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(other, 0o750); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	fi, err := os.Stat(other)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	fixEntry(other, fi, quietLogger())

	// The bob-specific arm keys strictly on the ".bob" basename, so this dir
	// must not have gained the group write bit from that arm.
	if got := statMode(t, other); got&bobStateGroupWriteBit != 0 && filepath.Base(other) != bobStateDirBase {
		// The general (owner-guarded) arm could add DirPerms only when the
		// process runs as DevUID; under test we are not DevUID, so no widening
		// should occur and the dir must stay 0750.
		if !runningAsDevUID() {
			t.Errorf("mode = %v, want non-.bob dir left at 0750 (bob arm must not touch it)", got)
		}
	}
}

// TestFixPermissionsRemediatesBobStateDirEndToEnd drives the full watcher tick:
// pointing WatchedHomeDirs at a temp tree containing a broken 0755 .bob dir, one
// fixPermissions pass must leave it group-writable, mirroring how the running
// watcher self-heals /data/agents/<name>/.bob and /data/home/.bob every tick.
func TestFixPermissionsRemediatesBobStateDirEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	root := filepath.Join(t.TempDir(), "data-agents")
	agentDir := filepath.Join(root, "scanner")
	if err := os.MkdirAll(agentDir, 0o775); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	bobDir := mkBobStateDir(t, agentDir, bobStateStartMode)

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{root}
	GooseLogsDir = filepath.Join(t.TempDir(), "goose-logs")
	t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

	if before := statMode(t, bobDir); before&bobStateDirGroupRWX == bobStateDirGroupRWX {
		t.Fatalf("precondition: %s already group-rwx at %v", bobDir, before)
	}

	fixPermissions(quietLogger())

	if got := statMode(t, bobDir); got&bobStateDirGroupRWX != bobStateDirGroupRWX {
		t.Errorf("after a watcher tick, %s mode = %v, want group r/w/x (%v)", bobDir, got, bobStateDirGroupRWX)
	}
}

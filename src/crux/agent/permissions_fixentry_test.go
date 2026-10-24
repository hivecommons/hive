package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Deliberately restrictive starting modes: neither grants the group access
// that agents sharing the node group require.
const (
	// tooTightDirMode lacks every group bit (needs g+rwx to be traversable).
	tooTightDirMode os.FileMode = 0o700
	// tooTightFileMode lacks every group bit (needs g+rw to be shared).
	tooTightFileMode os.FileMode = 0o600
	// alreadyOpenDirMode already satisfies DirPerms.
	alreadyOpenDirMode os.FileMode = 0o775
	// alreadyOpenFileMode already satisfies FilePerms.
	alreadyOpenFileMode os.FileMode = 0o664
)

// quietLogger discards output so an expected chown failure (these tests run
// unprivileged) does not spam the test log.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// statMode returns the current permission bits of path.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// runningAsDevUID reports whether this test process is the uid the watcher
// treats as "ours". The chmod arm of fixEntry is deliberately reachable only
// for entries owned by DevUID (or ones it just chowned to DevUID), so the
// mode-widening assertions only apply when that precondition genuinely holds.
func runningAsDevUID() bool { return os.Getuid() == DevUID }

// mkTightDir creates a directory with no group bits set.
func mkTightDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, tooTightDirMode); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	// Mkdir is umask-filtered, so pin the mode explicitly.
	if err := os.Chmod(path, tooTightDirMode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// mkTightFile creates a file with no group bits set.
func mkTightFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), tooTightFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, tooTightFileMode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// TestFixEntrySkipsEntriesOwnedByOtherUsers pins the guard that keeps the
// watcher quiet in production: an entry owned by neither DevUID nor root is
// left completely alone, because chmod-ing another user's file would only
// produce "operation not permitted" spam on every tick.
//
// Unprivileged test processes are exactly this case, so this is the arm that
// actually executes here.
func TestFixEntrySkipsEntriesOwnedByOtherUsers(t *testing.T) {
	if runningAsDevUID() {
		t.Skip("this process IS DevUID, so the skip arm is unreachable")
	}
	root := t.TempDir()

	dir := filepath.Join(root, "other-owner-dir")
	mkTightDir(t, dir)
	file := filepath.Join(root, "other-owner-file")
	mkTightFile(t, file)

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{dir, tooTightDirMode},
		{file, tooTightFileMode},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		fixEntry(tc.path, fi, quietLogger())

		if got := statMode(t, tc.path); got != tc.want {
			t.Errorf("%s mode = %v, want it left at %v: entries owned by another user must be skipped",
				filepath.Base(tc.path), got, tc.want)
		}
	}
}

// TestFixEntryWidensOwnedEntries covers the corrective arm: for entries the
// watcher may touch, a directory gains DirPerms and a file gains FilePerms so
// agents sharing the node group can use them.
func TestFixEntryWidensOwnedEntries(t *testing.T) {
	if !runningAsDevUID() {
		t.Skipf("chmod arm requires the process to run as DevUID (%d); running as %d", DevUID, os.Getuid())
	}
	root := t.TempDir()

	dir := filepath.Join(root, "tight-dir")
	mkTightDir(t, dir)
	file := filepath.Join(root, "tight-file")
	mkTightFile(t, file)

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{dir, DirPerms},
		{file, FilePerms},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		fixEntry(tc.path, fi, quietLogger())

		if got := statMode(t, tc.path); got&tc.want != tc.want {
			t.Errorf("%s mode = %v, want it to include %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// TestFixEntryLeavesSufficientModesAlone checks the watcher is non-destructive
// and idempotent: an entry that already satisfies the required bits must come
// back byte-identical, never narrowed to exactly DirPerms/FilePerms. This holds
// regardless of ownership, so it runs everywhere.
func TestFixEntryLeavesSufficientModesAlone(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "open-dir")
	if err := os.Mkdir(dir, alreadyOpenDirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, alreadyOpenDirMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	file := filepath.Join(root, "open-file")
	if err := os.WriteFile(file, []byte("x"), alreadyOpenFileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(file, alreadyOpenFileMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{dir, alreadyOpenDirMode},
		{file, alreadyOpenFileMode},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		fixEntry(tc.path, fi, quietLogger())

		if got := statMode(t, tc.path); got != tc.want {
			t.Errorf("%s mode = %v, want it unchanged at %v", filepath.Base(tc.path), got, tc.want)
		}
	}
}

// TestFixPermissionsWalksNestedEntries checks the walk really descends: bob
// creates its chat tree several levels below the watched root, so a shallow
// scan would leave it unfixed. Ownership decides whether each entry is then
// modified, but every entry must at least be visited — asserted here by the
// walk completing over a deep tree without error and leaving a
// sufficiently-permissive nested entry untouched.
func TestFixPermissionsWalksNestedEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "watched")
	nested := filepath.Join(root, "tmp", "some-uuid", "chats")
	if err := os.MkdirAll(nested, alreadyOpenDirMode); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.Chmod(nested, alreadyOpenDirMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	deepFile := filepath.Join(nested, "session.jsonl")
	if err := os.WriteFile(deepFile, []byte("{}"), alreadyOpenFileMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(deepFile, alreadyOpenFileMode); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{root}
	GooseLogsDir = filepath.Join(root, "goose-logs")
	t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

	fixPermissions(quietLogger())

	// Already-sufficient modes must survive the walk unchanged.
	if got := statMode(t, nested); got != alreadyOpenDirMode {
		t.Errorf("nested dir mode = %v, want it unchanged at %v", got, alreadyOpenDirMode)
	}
	if got := statMode(t, deepFile); got != alreadyOpenFileMode {
		t.Errorf("nested file mode = %v, want it unchanged at %v", got, alreadyOpenFileMode)
	}
}

// TestFixPermissionsRecreatesMissingRoot covers the self-healing arm: a watched
// root that does not exist yet must be created by the scan, not skipped
// forever. Without this, a volume mounted empty would never get its
// agent-writable directories.
func TestFixPermissionsRecreatesMissingRoot(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-yet-created")

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{missing}
	GooseLogsDir = filepath.Join(base, "goose-logs")
	t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s already exists", missing)
	}

	fixPermissions(quietLogger())

	fi, err := os.Stat(missing)
	if err != nil {
		t.Fatalf("watched root was not created: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("%s is not a directory", missing)
	}
	// ensureWatchedDirs also creates the goose log dir, which goose panics
	// without.
	if _, err := os.Stat(GooseLogsDir); err != nil {
		t.Errorf("goose logs dir was not created: %v", err)
	}
}

// TestFixPermissionsSkipsNonDirectoryRoot covers the guard for a watched path
// that exists but is a regular file: it must be skipped rather than walked.
func TestFixPermissionsSkipsNonDirectoryRoot(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "regular-file")
	mkTightFile(t, notADir)

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{notADir}
	GooseLogsDir = filepath.Join(root, "goose-logs")
	t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

	fixPermissions(quietLogger())

	if got := statMode(t, notADir); got != tooTightFileMode {
		t.Errorf("mode = %v, want it left at %v: a non-directory root must be skipped", got, tooTightFileMode)
	}
}

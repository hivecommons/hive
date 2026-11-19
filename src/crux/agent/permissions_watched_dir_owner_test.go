package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const (
	watchedDirChownFailedMsg = "permissions watcher: failed to chown directory"
	watchedDirChownSkipMsg   = "permissions watcher: skipping chown of watched directory owned by another uid"
)

// withWatcherIdentity points DevUID/NodeGID at uid/gid for the test and
// restores them, plus pristine dedupe state, on cleanup.
func withWatcherIdentity(t *testing.T, uid, gid int) {
	t.Helper()
	origUID, origGID := DevUID, NodeGID
	DevUID, NodeGID = uid, gid
	resetPermWarnDedupe()
	t.Cleanup(func() {
		DevUID, NodeGID = origUID, origGID
		resetPermWarnDedupe()
	})
}

// TestEnsureWatchedDirOwnerForeignOwnerSkipsWithoutWarn is the #18234
// regression: a watched root owned by another uid (a root-seeded /data/beads)
// must not be chowned by an unprivileged watcher, so the repeated
// "failed to chown directory" WARN never fires — however many times the
// startup path runs.
func TestEnsureWatchedDirOwnerForeignOwnerSkipsWithoutWarn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: every chown is permitted, skip arm unreachable")
	}
	// A target identity this process does not hold: were the chown attempted,
	// it would certainly fail with EPERM and log the WARN under test.
	withWatcherIdentity(t, os.Geteuid()+1, os.Getgid()+1)

	dir := filepath.Join(t.TempDir(), "beads")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	real, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	foreign := fakeOwnerInfo{FileInfo: real, sys: syntheticStat(t, dir, uint32(os.Geteuid()+1), 0)}

	h := &recordingHandler{}
	logger := slog.New(h)
	for range 10 {
		ensureWatchedDirOwner(dir, foreign, logger)
	}

	if got := h.count(slog.LevelWarn, watchedDirChownFailedMsg, ""); got != 0 {
		t.Errorf("WARN %q count = %d, want 0 for a dir only root can chown", watchedDirChownFailedMsg, got)
	}
	if got := h.count(slog.LevelDebug, watchedDirChownFailedMsg, ""); got != 0 {
		t.Errorf("chown attempted %d times on a foreign-owned dir, want 0", got)
	}
	if got := h.count(slog.LevelDebug, watchedDirChownSkipMsg, dir); got != 10 {
		t.Errorf("DEBUG skip count = %d, want 10", got)
	}
}

// TestEnsureWatchedDirOwnerOwnedDirStillChowned pins that the guard leaves
// the dirs this process owns alone: they are chowned exactly as before, with
// no skip line and no warning.
func TestEnsureWatchedDirOwnerOwnedDirStillChowned(t *testing.T) {
	withWatcherIdentity(t, os.Getuid(), os.Getgid())

	dir := t.TempDir()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	h := &recordingHandler{}
	ensureWatchedDirOwner(dir, fi, slog.New(h))

	if got := h.count(slog.LevelWarn, watchedDirChownFailedMsg, ""); got != 0 {
		t.Errorf("WARN count = %d, want 0 for a chown to our own identity", got)
	}
	if got := h.count(slog.LevelDebug, watchedDirChownSkipMsg, ""); got != 0 {
		t.Errorf("owned dir was skipped (%d skip lines), want it chowned", got)
	}
}

// TestEnsureWatchedDirOwnerOwnedDirChownFailureStillWarns pins that the guard
// hides only the certainly-unrepairable case: a dir this process owns whose
// chown still fails (here: to an identity it does not hold) warns once, and
// identical repeats are demoted by the existing deduper.
func TestEnsureWatchedDirOwnerOwnedDirChownFailureStillWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chown cannot fail, branch unreachable")
	}
	withWatcherIdentity(t, os.Geteuid()+1, os.Getgid()+1)

	dir := t.TempDir()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	h := &recordingHandler{}
	logger := slog.New(h)
	ensureWatchedDirOwner(dir, fi, logger)
	ensureWatchedDirOwner(dir, fi, logger)

	if got := h.count(slog.LevelWarn, watchedDirChownFailedMsg, dir); got != 1 {
		t.Errorf("WARN count = %d, want 1 (first failure warns, repeat demoted)", got)
	}
}

// TestEnsureWatchedDirOwnerNilInfoFallsThroughToChown covers the stat-failed
// arm: with no FileInfo the owner is unknown, so the chown is attempted and
// its own error is what gets reported.
func TestEnsureWatchedDirOwnerNilInfoFallsThroughToChown(t *testing.T) {
	withWatcherIdentity(t, os.Getuid(), os.Getgid())

	missing := filepath.Join(t.TempDir(), "vanished")
	h := &recordingHandler{}
	ensureWatchedDirOwner(missing, nil, slog.New(h))

	if got := h.count(slog.LevelWarn, watchedDirChownFailedMsg, missing); got != 1 {
		t.Errorf("WARN count = %d, want 1 for a chown of a missing dir", got)
	}
}

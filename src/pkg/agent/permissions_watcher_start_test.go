package agent

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// watcherPollTimeout bounds how long a test waits for the watcher goroutine
// to produce an observable effect before failing. Generous next to the
// shortened tick interval so slow CI never flakes.
const watcherPollTimeout = 5 * time.Second

// watcherTestInterval is the shortened tick used by these tests, small enough
// that a tick-driven repair lands well inside watcherPollTimeout.
const watcherTestInterval = 20 * time.Millisecond

// startWatcherSandbox repoints every path seam the watcher scans at a temp
// tree, shortens the tick interval, installs a stop channel, and restores all
// of it on cleanup. It returns the sandbox root and the stop channel. The
// stop channel is closed (and the goroutine given a moment to exit) BEFORE
// the seams are restored, so a still-ticking watcher can never touch the
// production paths or another test's sandbox (#4737-class hazard).
func startWatcherSandbox(t *testing.T) (root string, stop chan struct{}) {
	t.Helper()
	root = t.TempDir()

	origWatched := WatchedHomeDirs
	origGoose := GooseLogsDir
	origRepoParent := SharedRepoParent
	origModeGlob := ModeFileGlob
	origCapsGlob := CapsFileGlob
	origInterval := PermissionFixInterval
	origStop := permissionsWatcherStop
	origUID, origGID := DevUID, NodeGID

	WatchedHomeDirs = []string{filepath.Join(root, "home", ".claude")}
	GooseLogsDir = filepath.Join(root, "home", ".local", "state", "goose", "logs", "cli")
	SharedRepoParent = filepath.Join(root, "home")
	ModeFileGlob = filepath.Join(root, ".hive-mode-*")
	CapsFileGlob = filepath.Join(root, ".hive-caps-*")
	PermissionFixInterval = watcherTestInterval
	// Chown to our own identity so ensureWatchedDirs succeeds unprivileged.
	DevUID, NodeGID = os.Getuid(), os.Getgid()

	stop = make(chan struct{})
	permissionsWatcherStop = stop
	resetPermWarnDedupe()

	t.Cleanup(func() {
		// Stop the goroutine first; only then restore the seams it reads.
		select {
		case <-stop:
			// already closed by the test body
		default:
			close(stop)
		}
		// One shortened interval is ample for the select to observe the close.
		time.Sleep(2 * watcherTestInterval)
		WatchedHomeDirs = origWatched
		GooseLogsDir = origGoose
		SharedRepoParent = origRepoParent
		ModeFileGlob = origModeGlob
		CapsFileGlob = origCapsGlob
		PermissionFixInterval = origInterval
		permissionsWatcherStop = origStop
		DevUID, NodeGID = origUID, origGID
		resetPermWarnDedupe()
	})
	return root, stop
}

// waitFor polls cond until it holds or watcherPollTimeout elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(watcherPollTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(watcherTestInterval / 2)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestStartPermissionsWatcherCreatesDirsAndTicksRepairs pins the watcher's
// two documented duties end to end: on startup it creates every watched
// directory (ensureWatchedDirs), and on each tick it repairs what it finds
// (fixPermissions — proven here through the mode-file scan, whose effect is
// observable unprivileged: a 0600 mode file must come back world-readable so
// per-agent UIDs can read their GitHub mode, #3679/#3881/#3882).
func TestStartPermissionsWatcherCreatesDirsAndTicksRepairs(t *testing.T) {
	root, _ := startWatcherSandbox(t)

	go StartPermissionsWatcher(quietLogger())

	// Startup duty: watched dirs (including GooseLogsDir — goose panics
	// without it) exist without waiting for any tick.
	for _, dir := range append([]string{GooseLogsDir}, WatchedHomeDirs...) {
		dir := dir
		waitFor(t, "watched dir "+dir, func() bool {
			fi, err := os.Stat(dir)
			return err == nil && fi.IsDir()
		})
	}

	// Tick duty: a broken (owner-only) mode file planted AFTER startup is
	// only ever repaired by the ticker loop calling fixPermissions.
	path := mkModeFile(t, root, "watcher-start", modeFileStartMode)
	waitFor(t, "mode file widened to a+r by a watcher tick", func() bool {
		fi, err := os.Stat(path)
		return err == nil && fi.Mode().Perm()&modeFileReadBits == modeFileReadBits
	})
}

// TestStartPermissionsWatcherStopSeamEndsLoop pins the test seam's contract:
// closing permissionsWatcherStop makes the goroutine return, after which no
// further ticks repair anything — a mode file broken after the stop stays
// broken. Without this guarantee every test that starts the watcher would
// leak a ticker mutating whatever the package-level seams point at next.
func TestStartPermissionsWatcherStopSeamEndsLoop(t *testing.T) {
	root, stop := startWatcherSandbox(t)

	done := make(chan struct{})
	go func() {
		StartPermissionsWatcher(quietLogger())
		close(done)
	}()

	// Let it start (dirs appear), then stop it.
	waitFor(t, "watcher startup", func() bool {
		_, err := os.Stat(WatchedHomeDirs[0])
		return err == nil
	})
	close(stop)

	select {
	case <-done:
	case <-time.After(watcherPollTimeout):
		t.Fatal("StartPermissionsWatcher did not return after stop channel closed")
	}

	// A file broken after the loop returned must never be repaired.
	path := mkModeFile(t, root, "after-stop", modeFileStartMode)
	time.Sleep(5 * watcherTestInterval)
	if got := statMode(t, path); got != modeFileStartMode {
		t.Errorf("mode file changed to %v after watcher stopped; want %v untouched", got, modeFileStartMode)
	}
}

// TestEnsureWatchedDirsMkdirFailureWarnsAndContinues covers the mkdir failure
// arm: a watched "directory" whose parent is a regular file cannot be created
// (ENOTDIR), which must be logged-and-skipped — never a panic or abort — and
// must not prevent the remaining watched dirs from being created.
func TestEnsureWatchedDirsMkdirFailureWarnsAndContinues(t *testing.T) {
	root := t.TempDir()
	blockerParent := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blockerParent, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", blockerParent, err)
	}
	blocked := filepath.Join(blockerParent, "child") // MkdirAll fails: ENOTDIR
	healthy := filepath.Join(root, "healthy")

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	origUID, origGID := DevUID, NodeGID
	WatchedHomeDirs = []string{blocked, healthy}
	GooseLogsDir = filepath.Join(root, "goose")
	DevUID, NodeGID = os.Getuid(), os.Getgid()
	t.Cleanup(func() {
		WatchedHomeDirs, GooseLogsDir = origWatched, origGoose
		DevUID, NodeGID = origUID, origGID
		resetPermWarnDedupe()
	})
	resetPermWarnDedupe()

	ensureWatchedDirs(quietLogger())

	if _, err := os.Stat(blocked); !errors.Is(err, os.ErrNotExist) && err == nil {
		t.Errorf("blocked dir %s unexpectedly exists", blocked)
	}
	for _, dir := range []string{healthy, GooseLogsDir} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("dir %s not created despite earlier mkdir failure (err=%v)", dir, err)
		}
	}
}

// TestEnsureWatchedDirsChownFailureWarnsAndContinues covers the chown failure
// arm: an unprivileged process cannot chown to a uid it doesn't own, so with
// production DevUID/NodeGID this branch fires — and the dir must still exist
// (mkdir succeeded; only the ownership fix is best-effort).
func TestEnsureWatchedDirsChownFailureWarnsAndContinues(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown cannot fail, branch unreachable")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "watched")

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	origUID, origGID := DevUID, NodeGID
	WatchedHomeDirs = []string{dir}
	GooseLogsDir = filepath.Join(root, "goose")
	// A uid/gid this process does not hold: chown must fail with EPERM.
	DevUID, NodeGID = os.Getuid()+1, os.Getgid()+1
	t.Cleanup(func() {
		WatchedHomeDirs, GooseLogsDir = origWatched, origGoose
		DevUID, NodeGID = origUID, origGID
		resetPermWarnDedupe()
	})
	resetPermWarnDedupe()

	ensureWatchedDirs(quietLogger())

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("dir %s not created despite chown failure (err=%v)", dir, err)
	}
}

// TestWarnDedupedDemotesIdenticalRepeats covers warnDeduped's repeat arm:
// the first failure for an (op, path, error) triple logs at WARN, an
// identical repeat is demoted (the #4488 fix — six WARN lines every 10s
// dominated a healthy hive's log), and a changed error text warns again.
func TestWarnDedupedDemotesIdenticalRepeats(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)

	h := &recordingHandler{}
	logger := slog.New(h)
	err := errors.New("chmod /x: operation not permitted")
	const msg = "repair failed"

	warnDeduped(logger, "op", msg, "/x", err)
	warnDeduped(logger, "op", msg, "/x", err)
	if got := h.count(slog.LevelWarn, msg, "/x"); got != 1 {
		t.Errorf("WARN count after identical repeat = %d, want 1 (repeat demoted to debug)", got)
	}
	if got := h.count(slog.LevelDebug, msg, "/x"); got != 1 {
		t.Errorf("DEBUG count after identical repeat = %d, want 1", got)
	}

	// New information — the error text changed — must WARN again.
	warnDeduped(logger, "op", msg, "/x", errors.New("different failure"))
	if got := h.count(slog.LevelWarn, msg, "/x"); got != 2 {
		t.Errorf("WARN count after changed error = %d, want 2", got)
	}
}

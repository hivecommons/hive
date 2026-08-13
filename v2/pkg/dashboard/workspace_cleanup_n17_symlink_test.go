package dashboard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// AUDIT N17 (open across three audits): the chmod-and-retry fallback in
// removeTree walked an agent-writable workspace calling bare os.Chmod, which
// FOLLOWS symlinks. A symlink planted in the workspace pointing anywhere the
// cleanup user could chmod got its TARGET relaxed to 0o660 (or 0o770 for a
// directory) — turning "delete a stale workspace" into an arbitrary
// permission-loosening primitive on files entirely outside that workspace.
//
// TestN17ChmodWalkDoesNotFollowSymlinks is the regression.
// TestN17ChmodWalkStillRelaxesRealFiles is the POSITIVE CONTROL: an
// implementation that skipped the chmod entirely (or that failed the walk
// outright) would pass the regression while silently breaking the only reason
// the fallback exists.

// n17MakeStubbornDir builds a directory tree that RemoveAll cannot delete on the
// first attempt, forcing removeTree down the chmod-and-retry path. A directory
// with no write bit cannot have its children unlinked.
//
// links are created INSIDE the locked directory, and that placement is
// load-bearing. removeTree's first step is os.RemoveAll, which is destructive
// EVEN WHEN IT FAILS: it unlinks everything it can reach before hitting the
// unremovable child. A symlink sitting at the top of the workspace is therefore
// already gone by the time the chmod walk runs, and the test passes vacuously.
// Inside the locked directory the link survives step 1 and is still present when
// the walk — the code actually under test — reaches it.
func n17MakeStubbornDir(t *testing.T, links map[string]string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "workspace")
	inner := filepath.Join(work, "locked")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "file.txt"), []byte("x"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(inner, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	// Clamp the write bit off the inner dir so its child cannot be unlinked. This
	// is what makes the FIRST os.RemoveAll fail and drives removeTree down to the
	// chmod-and-retry walk — the code path under test. Without it removeTree
	// succeeds immediately, the walk never runs, and the symlink test goes green
	// no matter what the walk does. (That happened; the replay caught it.)
	if err := os.Chmod(inner, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(inner, 0o700) })

	// NOTE: do NOT "verify the premise" here by calling os.RemoveAll(work).
	// RemoveAll is destructive even when it FAILS — it deletes everything it can
	// reach before hitting the unremovable child, which includes the planted
	// symlinks. The walk under test then finds nothing and the test passes
	// vacuously. (Exactly this masked the replay until it was traced.) The
	// premise is instead asserted non-destructively by the caller, on a
	// throwaway copy of the same shape.
	return work
}

// n17AssertRemoveAllFails checks, on a SEPARATE throwaway tree of the same
// shape, that a plain os.RemoveAll really does fail — i.e. that removeTree will
// be forced down the chmod-and-retry walk. Done on a copy precisely because the
// check destroys what it touches.
func n17AssertRemoveAllFails(t *testing.T) {
	t.Helper()
	probe := n17MakeStubbornDir(t, nil)
	if err := os.RemoveAll(probe); err == nil {
		t.Fatal("test premise broken: plain RemoveAll deleted the tree, so the " +
			"chmod walk under test never runs and this test proves nothing")
	}
}

// TestN17ChmodWalkDoesNotFollowSymlinks is the regression. A symlink inside the
// doomed workspace points at a file OUTSIDE it whose mode is deliberately tight
// (0o400). After removeTree runs, that outside file's mode must be untouched.
func TestN17ChmodWalkDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and symlinks")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so RemoveAll never needs the chmod walk")
	}

	// The victim lives outside the tree being removed — this is the whole point:
	// nothing in the cleanup's remit should be able to reach it.
	victimDir := t.TempDir()
	victimFile := filepath.Join(victimDir, "secret.key")
	if err := os.WriteFile(victimFile, []byte("private"), 0o400); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	victimSubdir := filepath.Join(victimDir, "protected")
	if err := os.Mkdir(victimSubdir, 0o500); err != nil {
		t.Fatalf("mkdir victim dir: %v", err)
	}

	n17AssertRemoveAllFails(t)

	// The planted links. The walk will visit both; a following chmod relaxes the
	// TARGETS rather than the links.
	work := n17MakeStubbornDir(t, map[string]string{
		"link-to-secret": victimFile,
		"link-to-dir":    victimSubdir,
	})

	// removeTree's success/failure is not what is under test — the side effect
	// on the victim is. Deliberately ignore the error.
	_ = removeTree(work)

	fi, err := os.Lstat(victimFile)
	if err != nil {
		t.Fatalf("victim file vanished: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o400 {
		t.Errorf("symlinked-to file mode = %#o, want 0400 unchanged; "+
			"the cleanup walk followed a symlink out of the workspace (N17)", got)
	}

	di, err := os.Lstat(victimSubdir)
	if err != nil {
		t.Fatalf("victim dir vanished: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o500 {
		t.Errorf("symlinked-to dir mode = %#o, want 0500 unchanged; "+
			"the cleanup walk followed a symlink out of the workspace (N17)", got)
	}

	// And the links themselves must not have dragged the victims into deletion.
	if _, err := os.Stat(victimFile); err != nil {
		t.Errorf("victim file should still exist: %v", err)
	}
}

// TestN17ChmodWalkStillRelaxesRealFiles is the positive control: the fallback
// must still do its job on REAL (non-symlink) entries, i.e. a read-only tree
// that RemoveAll alone chokes on is still removed. "Skip everything" would pass
// the regression above and fail here.
func TestN17ChmodWalkStillRelaxesRealFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so RemoveAll never needs the fallback")
	}

	work := n17MakeStubbornDir(t, nil)
	if err := removeTree(work); err != nil {
		t.Fatalf("removeTree should still remove a read-only tree via the chmod "+
			"fallback; got %v", err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("workspace should be gone, stat err = %v", err)
	}
}

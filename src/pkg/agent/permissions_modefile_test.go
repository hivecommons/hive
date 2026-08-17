package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// modeFileStartMode is the exact production-observed broken mode: a mode file
// written owner-only (0600 — the #3172 build, or a umask-narrowed write) that
// the per-agent UIDs running gh-wrapper.sh / git-credential-hive.sh could not
// read, so every gh call and push was blocked (#3679/#3881/#3882).
const modeFileStartMode os.FileMode = 0o600

// withModeFileGlob points the watcher's mode-file scan at a temp pattern for
// the duration of the test, restoring the production value afterwards.
func withModeFileGlob(t *testing.T, glob string) {
	t.Helper()
	orig := ModeFileGlob
	ModeFileGlob = glob
	t.Cleanup(func() { ModeFileGlob = orig })
}

// mkModeFile creates a .hive-mode-<agent> file pinned to a given mode
// (os.WriteFile is umask-filtered, so the mode is re-applied explicitly —
// which is precisely one of the production bugs this test file exists for).
func mkModeFile(t *testing.T, dir, agent string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, ".hive-mode-"+agent)
	if err := os.WriteFile(path, []byte("ISSUES_AND_PRS"), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

// TestFixModeFilesWidensUnreadableFile is the core fail-before/pass-after: a
// mode file created 0600 must come back world-readable so the enforcement
// scripts running as per-agent UIDs can read it.
func TestFixModeFilesWidensUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := mkModeFile(t, dir, "quality", modeFileStartMode)
	withModeFileGlob(t, filepath.Join(dir, ".hive-mode-*"))

	fixModeFiles(quietLogger())

	got := statMode(t, path)
	if got&modeFileReadBits != modeFileReadBits {
		t.Errorf("mode = %v, want a+r (%v) so agent UIDs can read their GitHub mode", got, modeFileReadBits)
	}
	if got != modeFileStartMode|modeFileReadBits {
		t.Errorf("mode = %v, want exactly %v (only read bits added, write bits preserved)", got, modeFileStartMode|modeFileReadBits)
	}
}

// TestFixModeFilesLeavesCorrectFileUntouched: an already world-readable 0644
// file must stay byte-identical so the scan is idempotent.
func TestFixModeFilesLeavesCorrectFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := mkModeFile(t, dir, "guide", 0o644)
	withModeFileGlob(t, filepath.Join(dir, ".hive-mode-*"))

	fixModeFiles(quietLogger())

	if got := statMode(t, path); got != 0o644 {
		t.Errorf("mode = %v, want 0644 unchanged", got)
	}
}

// TestFixModeFilesSkipsSymlink: /tmp is sticky and world-writable, so an agent
// can plant a symlink matching the glob; a path-based chmod would follow it
// (CWE-59), so the scan opens O_NOFOLLOW and must never act on one. The link
// target must keep its mode.
func TestFixModeFilesSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod %s: %v", target, err)
	}
	link := filepath.Join(dir, ".hive-mode-evil")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	withModeFileGlob(t, filepath.Join(dir, ".hive-mode-*"))

	fixModeFiles(quietLogger())

	if got := statMode(t, target); got != 0o600 {
		t.Errorf("symlink target mode = %v, want 0600 untouched (watcher must not follow planted links)", got)
	}
}

// TestFixModeFilesSkipsNonRegular: a directory planted at a matching name is
// not a mode file and must be left alone (only regular files are ever chmod'd).
func TestFixModeFilesSkipsNonRegular(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, ".hive-mode-dir")
	if err := os.Mkdir(planted, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(planted, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	withModeFileGlob(t, filepath.Join(dir, ".hive-mode-*"))

	fixModeFiles(quietLogger())

	if got := statMode(t, planted); got != 0o700 {
		t.Errorf("planted dir mode = %v, want 0700 untouched", got)
	}
}

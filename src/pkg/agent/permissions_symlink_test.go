package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type symlinkFileInfo struct {
	os.FileInfo
	mode os.FileMode
	stat *syscall.Stat_t
}

func (fi symlinkFileInfo) Mode() os.FileMode { return fi.mode }
func (fi symlinkFileInfo) Sys() any          { return fi.stat }

func ownedDevNodeSymlinkInfo(t *testing.T, link string, mode os.FileMode) os.FileInfo {
	t.Helper()

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("precondition: %s was not reported as a symlink", link)
	}
	return symlinkFileInfo{
		FileInfo: fi,
		mode:     os.ModeSymlink | mode,
		stat: &syscall.Stat_t{
			Uid: uint32(DevUID),
			Gid: uint32(NodeGID),
		},
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	if got := statMode(t, path); got != want {
		t.Fatalf("%s mode = %v, want %v: fixEntry followed a symlink (CWE-59 regression)",
			filepath.Base(path), got, want)
	}
}

// TestFixEntrySkipsSymlinks pins the CWE-59 guard added in PR #3432: a planted
// symlink in the agent HOME tree must never be followed, because both os.Chmod
// and os.Chown follow symlinks — following one would redirect the repair loop
// onto a file outside the watched tree, allowing a local attacker to escalate
// permissions on an arbitrary path.
func TestFixEntrySkipsSymlinks(t *testing.T) {
	root := t.TempDir()

	target := filepath.Join(root, "outside-target")
	if err := os.WriteFile(target, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	link := filepath.Join(root, "planted-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fixEntry(link, ownedDevNodeSymlinkInfo(t, link, 0o600), quietLogger())

	assertMode(t, target, 0o600)
}

func TestFixEntrySkipsRelativeSymlinkToOutsideRoot(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	link := filepath.Join(watched, "relative-link")
	if err := os.Symlink(filepath.Join("..", "outside", "secret"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fixEntry(link, ownedDevNodeSymlinkInfo(t, link, 0o600), quietLogger())

	assertMode(t, target, 0o600)
}

func TestFixEntrySkipsSymlinkToDirectory(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	targetDir := filepath.Join(root, "outside-dir")
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	if err := os.Chmod(targetDir, 0o700); err != nil {
		t.Fatalf("chmod target dir: %v", err)
	}

	link := filepath.Join(watched, "dir-link")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	fixEntry(link, ownedDevNodeSymlinkInfo(t, link, 0o700), quietLogger())

	assertMode(t, targetDir, 0o700)
}

// TestFixPermissionsDoesNotFollowSymlinkedParentInWalk exercises the full
// fixPermissions walk with a symlinked parent directory planted inside a
// watched root. filepath.Walk must visit the link itself via Lstat, not descend
// through it to the outside target tree.
func TestFixPermissionsDoesNotFollowSymlinkedParentInWalk(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	parentLink := filepath.Join(watched, "symlinked-parent")
	if err := os.Symlink(outside, parentLink); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}

	origWatched, origGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{watched}
	GooseLogsDir = filepath.Join(root, "goose-logs")
	t.Cleanup(func() { WatchedHomeDirs = origWatched; GooseLogsDir = origGoose })

	fixPermissions(quietLogger())

	assertMode(t, target, 0o600)
}

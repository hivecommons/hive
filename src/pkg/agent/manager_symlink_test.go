package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func quietManager() *Manager {
	return &Manager{logger: quietLogger()}
}

func assertWorldWritableTargetMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	if got := statMode(t, path); got != want {
		t.Fatalf("%s mode = %v, want %v: ensureWorldWritable followed a symlink (CWE-59 regression)",
			filepath.Base(path), got, want)
	}
}

func TestEnsureWorldWritableSkipsSymlinkToOutsideFile(t *testing.T) {
	root := t.TempDir()

	target := filepath.Join(root, "outside-target")
	if err := os.WriteFile(target, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	watched := filepath.Join(root, "watched")
	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	link := filepath.Join(watched, "planted-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	quietManager().ensureWorldWritable(watched)

	assertWorldWritableTargetMode(t, target, 0o600)
}

func TestEnsureWorldWritableSkipsRelativeSymlinkToOutsideFile(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
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

	quietManager().ensureWorldWritable(watched)

	assertWorldWritableTargetMode(t, target, 0o600)
}

func TestEnsureWorldWritableSkipsSymlinkToOutsideDirectory(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	targetDir := filepath.Join(root, "outside-dir")
	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatalf("mkdir watched: %v", err)
	}
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	if err := os.Chmod(targetDir, 0o700); err != nil {
		t.Fatalf("chmod target dir: %v", err)
	}

	link := filepath.Join(watched, "dir-link")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	quietManager().ensureWorldWritable(watched)

	assertWorldWritableTargetMode(t, targetDir, 0o700)
}

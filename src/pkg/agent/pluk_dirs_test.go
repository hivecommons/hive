package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func setUmask(mask int) int {
	return syscall.Umask(mask)
}

func TestEnsurePlukRunDirsUsesGroupOnlySetgidMode(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "pluk")
	oldUmask := setUmask(0o077)
	defer setUmask(oldUmask)

	if err := ensurePlukRunDirs(runDir); err != nil {
		t.Fatalf("ensure pluk dirs: %v", err)
	}

	for _, name := range []string{"logs", "commands"} {
		path := filepath.Join(runDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", path)
		}
		if got := info.Mode().Perm(); got != 0o770 {
			t.Errorf("%s mode = %04o, want 0770", path, got)
		}
		if info.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s mode missing setgid bit: %s", path, info.Mode())
		}
		if info.Mode().Perm()&0o002 != 0 {
			t.Errorf("%s is world-writable: %s", path, info.Mode())
		}
	}
}

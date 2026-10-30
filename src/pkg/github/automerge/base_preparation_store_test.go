package automerge

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestBasePreparationStoreFileModes verifies the store never creates
// group-writable state on the shared /data volume. The umask is cleared so the
// requested modes land on disk unmasked (runners default to 022, which would
// hide a group-writable request), and the store root is a subdirectory of
// t.TempDir() so MkdirAll actually creates it with the store's mode.
func TestBasePreparationStoreFileModes(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "state")
	store := NewBasePreparationStore(dir)

	err := store.change("owner/repo:main", func(st *basePreparation) error {
		st.Owner.Number = 42
		return nil
	})
	if err != nil {
		t.Fatalf("change failed: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat directory failed: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o755 {
		t.Errorf("directory mode = %#o, want 0755", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir failed: %v", err)
	}
	var sawJSON, sawLock bool
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("info %s failed: %v", e.Name(), err)
		}
		switch {
		case strings.HasSuffix(e.Name(), ".json"):
			sawJSON = true
		case strings.HasSuffix(e.Name(), ".lock"):
			sawLock = true
		default:
			t.Errorf("unexpected entry %q", e.Name())
			continue
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("%s mode = %#o, want 0644", e.Name(), got)
		}
	}
	if !sawJSON || !sawLock {
		t.Fatalf("expected a .json and a .lock entry, got json=%v lock=%v", sawJSON, sawLock)
	}
}

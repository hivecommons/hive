package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPruneCopilotCacheOnlyRemovesStaleScratchDirs(t *testing.T) {
	root := t.TempDir()
	oldTime := time.Now().Add(-2 * copilotCachePruneAge)
	newTime := time.Now()
	paths := map[string]bool{
		filepath.Join(root, "tmp", "old-tmp"):                        true,
		filepath.Join(root, "tmp", "new-tmp"):                        false,
		filepath.Join(root, "linux-x64", ".extracting-1.0.88-1-2-3"): true,
		filepath.Join(root, "linux-x64", ".extracting-1.0.88-new"):   false,
		filepath.Join(root, "linux-x64", "1.0.88"):                   false,
	}
	for path, shouldPrune := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := newTime
		if shouldPrune {
			ts = oldTime
		}
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := pruneCopilotCache(root, time.Now().Add(-copilotCachePruneAge))
	if err != nil {
		t.Fatalf("pruneCopilotCache error = %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for path, shouldPrune := range paths {
		_, err := os.Stat(path)
		if shouldPrune && !os.IsNotExist(err) {
			t.Fatalf("%s still exists, want pruned", path)
		}
		if !shouldPrune && err != nil {
			t.Fatalf("%s missing unexpectedly: %v", path, err)
		}
	}
}

func TestRunCopilotCacheJanitorUsesSharedHome(t *testing.T) {
	oldHome := sharedAgentHome
	sharedAgentHome = filepath.Join(t.TempDir(), "home")
	t.Cleanup(func() { sharedAgentHome = oldHome })
	oldScratch := filepath.Join(copilotCacheRoot(), "linux-x64", ".extracting-1.0.88-old")
	if err := os.MkdirAll(oldScratch, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * copilotCachePruneAge)
	if err := os.Chtimes(oldScratch, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	NewManager(nil, discardLogger(), ProjectContext{}).runCopilotCacheJanitor()
	if _, err := os.Stat(oldScratch); !os.IsNotExist(err) {
		t.Fatalf("janitor left stale scratch dir: %v", err)
	}
}

func TestCopilotDiskFullConditionAndErrors(t *testing.T) {
	old := statfsForPath
	statfsForPath = func(_ string, st *syscall.Statfs_t) error {
		st.Blocks = 100
		st.Bavail = 4
		return nil
	}
	t.Cleanup(func() { statfsForPath = old })
	reason, full := copilotDiskFullCondition()
	if !full || !strings.Contains(reason, "96% full") {
		t.Fatalf("copilotDiskFullCondition = (%q, %v), want 96%% full", reason, full)
	}
	for _, msg := range []string{
		"TAR_ENTRY_ERROR(Unknown system error -122)",
		"write failed: ENOSPC",
		"disk quota exceeded",
	} {
		if !isCopilotDiskFullError(msg) {
			t.Fatalf("isCopilotDiskFullError(%q) = false, want true", msg)
		}
	}
}

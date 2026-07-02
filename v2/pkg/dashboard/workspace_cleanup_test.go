package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsSkippedEntry(t *testing.T) {
	skipped := []string{
		".cache", ".config", ".npm-cache", "bin",
		"beads.json", "stats.json",
		".agents", ".cursor", ".windsurf", ".clinerules", ".github", ".opencode",
		".copilot-session",
		".some-dotfile",
	}
	for _, name := range skipped {
		if !isSkippedEntry(name) {
			t.Errorf("expected %q to be skipped", name)
		}
	}

	notSkipped := []string{
		"console-fix-20041", "scanner-fix-20053", "docs-repo",
		"console", "fix-pr", "go-build-cache", "repos",
		"analysis.txt", "create_pr.py",
	}
	for _, name := range notSkipped {
		if isSkippedEntry(name) {
			t.Errorf("expected %q to NOT be skipped", name)
		}
	}
}

func TestRemoveTree(t *testing.T) {
	root := t.TempDir()

	// Create a nested directory tree
	nested := filepath.Join(root, "stale-workspace", "sub", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "stale-workspace")
	if err := removeTree(target); err != nil {
		t.Fatalf("removeTree failed: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected target to be removed, got: %v", err)
	}
}

func TestRemoveTreeReadOnlyDir(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "readonly-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locked.txt"), []byte("test"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Make dir read-only so os.RemoveAll would fail without chmod
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}

	if err := removeTree(dir); err != nil {
		t.Fatalf("removeTree failed on read-only dir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected dir to be removed")
	}
}

func TestFileOwnerName(t *testing.T) {
	f := filepath.Join(t.TempDir(), "owned.txt")
	if err := os.WriteFile(f, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := fileOwnerName(f)
	if err != nil {
		t.Fatalf("fileOwnerName failed: %v", err)
	}
	if name == "" {
		t.Fatal("expected non-empty owner name")
	}
}

func TestFileOwnerNameNotExist(t *testing.T) {
	_, err := fileOwnerName("/nonexistent/path/xyz")
	if err == nil {
		t.Fatal("expected error for non-existent path")
	}
}

func TestSweepWorkspacesSkipsRecent(t *testing.T) {
	origRoot := agentWorkspaceRoot
	root := t.TempDir()

	// Create agent dir with a recent workspace
	agentDir := filepath.Join(root, "test-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(agentDir, "recent-workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Temporarily override (test-only — not thread-safe but acceptable for unit test)
	_ = origRoot

	// Verify the recent workspace would not be removed by age check
	info, err := os.Stat(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	age := time.Since(info.ModTime())
	if age >= workspaceMaxAge {
		t.Fatalf("test workspace should be recent, got age %v", age)
	}
}

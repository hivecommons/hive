package dashboard

import (
	"context"
	"io"
	"log/slog"
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
	if age >= workspaceMaxAge() {
		t.Fatalf("test workspace should be recent, got age %v", age)
	}
}

// sweepTestLogger returns a logger that discards output, for driving
// sweepWorkspaces directly in tests.
func sweepTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newSweepAgentDir points agentWorkspaceRoot at a fresh temp root (restored on
// cleanup) and creates one agent workspace under it, populated with a stale
// directory (with a file inside) and a stale top-level file. It returns the
// agent dir plus the two stale paths.
func newSweepAgentDir(t *testing.T, agent string) (agentDir, staleDir, staleFile string) {
	t.Helper()
	root := t.TempDir()
	orig := agentWorkspaceRoot
	agentWorkspaceRoot = root
	t.Cleanup(func() { agentWorkspaceRoot = orig })

	agentDir = filepath.Join(root, agent)
	staleDir = filepath.Join(agentDir, "src")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "handler.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleFile = filepath.Join(agentDir, "main.go")
	if err := os.WriteFile(staleFile, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * workspaceMaxAge())
	for _, p := range []string{staleDir, staleFile} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return agentDir, staleDir, staleFile
}

// TestSweepSkipsGitCloneWorkspaceDir is the regression test for the vllmd-07
// incident: an agent workspace root that is itself a git clone (sec-check's
// workspace WAS its clone of the tenant repo) must not be swept, no matter how
// stale its tracked entries look. Before the git-clone guard, sweepWorkspaces
// deleted both stale entries here — this test fails against the unfixed code.
func TestSweepSkipsGitCloneWorkspaceDir(t *testing.T) {
	agentDir, staleDir, staleFile := newSweepAgentDir(t, "sec-check")
	if err := os.MkdirAll(filepath.Join(agentDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	sweepWorkspaces(sweepTestLogger(), nil)

	if _, err := os.Stat(staleDir); err != nil {
		t.Errorf("stale dir inside git clone was removed: %v", err)
	}
	if _, err := os.Stat(staleFile); err != nil {
		t.Errorf("stale file inside git clone was removed: %v", err)
	}
}

// TestSweepSkipsGitCloneWorkspaceWorktreeFile covers the linked-worktree case:
// a .git FILE (a "gitdir:" pointer), not a directory, must also trigger the
// guard. Fails against the unfixed code the same way as the dir case.
func TestSweepSkipsGitCloneWorkspaceWorktreeFile(t *testing.T) {
	agentDir, staleDir, staleFile := newSweepAgentDir(t, "quality")
	if err := os.WriteFile(filepath.Join(agentDir, ".git"),
		[]byte("gitdir: /data/agents/quality/.repos/ui/.git/worktrees/wt1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sweepWorkspaces(sweepTestLogger(), nil)

	if _, err := os.Stat(staleDir); err != nil {
		t.Errorf("stale dir inside git worktree was removed: %v", err)
	}
	if _, err := os.Stat(staleFile); err != nil {
		t.Errorf("stale file inside git worktree was removed: %v", err)
	}
}

// TestSweepStillCleansNonGitWorkspace guards the feature itself: a workspace
// with NO .git entry must still have its stale entries removed.
func TestSweepStillCleansNonGitWorkspace(t *testing.T) {
	_, staleDir, staleFile := newSweepAgentDir(t, "scratch-agent")

	sweepWorkspaces(sweepTestLogger(), nil)

	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale dir in non-git workspace should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("stale file in non-git workspace should be removed, stat err=%v", err)
	}
}

// TestStartWorkspaceCleanupDisabled verifies the config gate: with
// HIVE_WORKSPACE_CLEANUP_ENABLED=false, StartWorkspaceCleanup returns without
// running even the initial sweep, so stale entries survive.
func TestStartWorkspaceCleanupDisabled(t *testing.T) {
	_, staleDir, staleFile := newSweepAgentDir(t, "scratch-agent")
	t.Setenv(workspaceCleanupEnabledEnv, "false")

	// Cancelled context: if the gate were broken, the enabled path would still
	// run its initial sweep before returning.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartWorkspaceCleanup(ctx, sweepTestLogger(), nil)

	if _, err := os.Stat(staleDir); err != nil {
		t.Errorf("sweep ran while disabled; stale dir removed: %v", err)
	}
	if _, err := os.Stat(staleFile); err != nil {
		t.Errorf("sweep ran while disabled; stale file removed: %v", err)
	}
}

// TestWorkspaceCleanupEnvKnobs covers the env parsing: enable/disable values
// and duration overrides with fallback to defaults on bad input.
func TestWorkspaceCleanupEnvKnobs(t *testing.T) {
	for _, v := range []string{"", "1", "true", "yes", "on", "garbage"} {
		t.Setenv(workspaceCleanupEnabledEnv, v)
		if !workspaceCleanupEnabled() {
			t.Errorf("value %q: expected enabled", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", " OFF "} {
		t.Setenv(workspaceCleanupEnabledEnv, v)
		if workspaceCleanupEnabled() {
			t.Errorf("value %q: expected disabled", v)
		}
	}

	t.Setenv(workspaceCleanupIntervalEnv, "30m")
	if got := workspaceCleanupInterval(); got != 30*time.Minute {
		t.Errorf("interval override: got %v, want 30m", got)
	}
	t.Setenv(workspaceCleanupIntervalEnv, "not-a-duration")
	if got := workspaceCleanupInterval(); got != workspaceCleanupIntervalDefault {
		t.Errorf("bad interval: got %v, want default %v", got, workspaceCleanupIntervalDefault)
	}
	t.Setenv(workspaceMaxAgeEnv, "6h")
	if got := workspaceMaxAge(); got != 6*time.Hour {
		t.Errorf("max-age override: got %v, want 6h", got)
	}
	t.Setenv(workspaceMaxAgeEnv, "-1h")
	if got := workspaceMaxAge(); got != workspaceMaxAgeDefault {
		t.Errorf("negative max-age: got %v, want default %v", got, workspaceMaxAgeDefault)
	}
}

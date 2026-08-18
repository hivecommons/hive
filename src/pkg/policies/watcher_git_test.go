package policies

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStartWithLocalGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", out, err)
	}
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "test").Run()

	agentDir := filepath.Join(repoDir, "agents")
	os.MkdirAll(agentDir, 0o755)
	os.WriteFile(filepath.Join(agentDir, "scanner.md"), []byte("scan prompt"), 0o644)

	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()

	localDir := t.TempDir()
	w := NewWatcher(repoDir, "master", "agents", localDir, 5*time.Minute, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := w.Start(ctx); err != nil {
		branchOut, _ := exec.Command("git", "-C", repoDir, "branch").CombinedOutput()
		t.Logf("branches: %s", branchOut)
		t.Fatalf("Start: %v", err)
	}

	data, ok := w.GetPolicy("scanner")
	if !ok {
		t.Error("scanner policy not found")
	}
	if string(data) != "scan prompt" {
		t.Errorf("policy = %q", string(data))
	}

	cancel()
}

func TestInitialCloneExistingRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoDir := t.TempDir()
	exec.Command("git", "init", repoDir).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(repoDir, "test.md"), []byte("hello"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()

	localDir := t.TempDir()
	w := NewWatcher(repoDir, "master", "", localDir, time.Minute, slog.Default())

	if err := w.initialClone(); err != nil {
		branchOut, _ := exec.Command("git", "-C", repoDir, "branch").CombinedOutput()
		t.Logf("branches: %s", branchOut)
		t.Fatalf("initialClone: %v", err)
	}

	if _, err := os.Stat(filepath.Join(localDir, ".git")); err != nil {
		t.Error("cloned repo should have .git dir")
	}
}

func TestPullNoChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoDir := t.TempDir()
	exec.Command("git", "init", repoDir).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(repoDir, "test.md"), []byte("hello"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()

	localDir := t.TempDir()
	w := NewWatcher(repoDir, "master", "", localDir, time.Minute, slog.Default())
	w.initialClone()

	w.pull()
}

func TestPullWithNewCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoDir := t.TempDir()
	exec.Command("git", "init", repoDir).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(repoDir, "scanner.md"), []byte("v1"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()

	localDir := t.TempDir()
	w := NewWatcher(repoDir, "master", "", localDir, time.Minute, slog.Default())
	w.initialClone()
	w.loadPolicies()

	os.WriteFile(filepath.Join(repoDir, "scanner.md"), []byte("v2"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "update").Run()

	w.pull()

	data, ok := w.GetPolicy("scanner")
	if !ok {
		t.Error("scanner should exist after pull")
	}
	if string(data) != "v2" {
		t.Errorf("policy = %q, want v2", string(data))
	}
}

func TestStartLoadPoliciesError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoDir := t.TempDir()
	exec.Command("git", "init", repoDir).Run()
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "test").Run()
	os.WriteFile(filepath.Join(repoDir, "test.md"), []byte("hello"), 0o644)
	exec.Command("git", "-C", repoDir, "add", ".").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "init").Run()

	localDir := t.TempDir()
	w := NewWatcher(repoDir, "master", "nonexistent-subpath", localDir, time.Minute, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := w.Start(ctx)
	if err == nil {
		t.Error("expected error for nonexistent subpath")
	}
}

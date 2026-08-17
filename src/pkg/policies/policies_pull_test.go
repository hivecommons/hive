package policies

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPullAlreadyUpToDateExtra(t *testing.T) {
	// Create a bare repo and clone it
	bareDir := t.TempDir()
	cloneDir := t.TempDir()

	// Init bare repo
	cmd := exec.Command("git", "init", "--bare", bareDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init bare: %v\n%s", err, out)
	}

	// Clone it
	cmd = exec.Command("git", "clone", bareDir, cloneDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	// Create initial commit
	policyDir := filepath.Join(cloneDir, "policies")
	os.MkdirAll(policyDir, 0755)
	os.WriteFile(filepath.Join(policyDir, "scanner-policy.md"), []byte("# Scanner\nScan for bugs"), 0644)
	cmd = exec.Command("git", "-C", cloneDir, "add", ".")
	cmd.Run()
	cmd = exec.Command("git", "-C", cloneDir, "commit", "-m", "initial")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", cloneDir, "push", "origin", "HEAD:main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	w := NewWatcher(bareDir, "main", "policies", cloneDir, time.Minute, slog.Default())
	_ = w.loadPolicies()

	// Pull when already up to date
	w.pull()

	policies := w.AllPolicies()
	if len(policies) == 0 {
		t.Error("should have loaded policies after pull")
	}
}

func TestPullWithNewCommitExtra(t *testing.T) {
	bareDir := t.TempDir()
	cloneDir := t.TempDir()
	workDir := t.TempDir()

	cmd := exec.Command("git", "init", "--bare", bareDir)
	cmd.Run()
	cmd = exec.Command("git", "clone", bareDir, cloneDir)
	cmd.Run()

	policyDir := filepath.Join(cloneDir, "policies")
	os.MkdirAll(policyDir, 0755)
	os.WriteFile(filepath.Join(policyDir, "scanner-policy.md"), []byte("# Scanner v1"), 0644)
	cmd = exec.Command("git", "-C", cloneDir, "add", ".")
	cmd.Run()
	cmd = exec.Command("git", "-C", cloneDir, "commit", "-m", "v1")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
	cmd.Run()
	cmd = exec.Command("git", "-C", cloneDir, "push", "origin", "HEAD:main")
	cmd.Run()

	// Clone again to workDir (this simulates the watcher's local copy)
	cmd = exec.Command("git", "clone", bareDir, workDir)
	cmd.Run()

	// Push a new commit from cloneDir
	os.WriteFile(filepath.Join(policyDir, "scanner-policy.md"), []byte("# Scanner v2"), 0644)
	cmd = exec.Command("git", "-C", cloneDir, "add", ".")
	cmd.Run()
	cmd = exec.Command("git", "-C", cloneDir, "commit", "-m", "v2")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
	cmd.Run()
	cmd = exec.Command("git", "-C", cloneDir, "push", "origin", "HEAD:main")
	cmd.Run()

	w := NewWatcher(bareDir, "main", "policies", workDir, time.Minute, slog.Default())
	_ = w.loadPolicies()

	// Pull should fetch the new commit and reload
	w.pull()

	policies := w.AllPolicies()
	if len(policies) == 0 {
		t.Error("should have policies after pull with update")
	}
}

func TestPullBadDir(t *testing.T) {
	w := NewWatcher("", "main", "", "/nonexistent-dir-xyz", time.Minute, slog.Default())
	w.pull() // should not panic
}

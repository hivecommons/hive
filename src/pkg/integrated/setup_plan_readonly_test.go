package integrated

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadOnlySetupPlanLeavesRequestedStateUntouched(t *testing.T) {
	const repository = "owner/read-only-plan"
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runCheckoutFixtureGit(t, root, "init", "--bare", remote)
	runCheckoutFixtureGit(t, root, "init", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "package.json"), []byte(`{"scripts":{"test":"node --test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runCheckoutFixtureGit(t, seed, "add", "package.json")
	runCheckoutFixtureGit(t, seed, "-c", "user.name=Hive Test", "-c", "user.email=hive@example.invalid", "commit", "-m", "seed")
	runCheckoutFixtureGit(t, seed, "remote", "add", "origin", remote)
	runCheckoutFixtureGit(t, seed, "push", "-u", "origin", "main")
	runCheckoutFixtureGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	previousCloneURL := setupRepositoryCloneURL
	setupRepositoryCloneURL = func(string) string { return remote }
	t.Cleanup(func() { setupRepositoryCloneURL = previousCloneURL })

	stateDir := filepath.Join(root, "hive-read-only-state")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := RunSetup(ctx, SetupOptions{
		Repository: repository, Coverage: CoverageEssential, Automation: AutomationRepairPR,
		Provider: "codex", StateDir: stateDir, MaxActiveIssues: 5, MaxRepairAttempts: 4,
		ExecutionMode: ExecutionLocal, VisualHive: true, VisualHiveRepo: "owner/visual-hive",
		VisualHiveRef: strings.Repeat("a", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Plan.ReadOnly || result.Plan.Repository != repository {
		t.Fatalf("unexpected read-only plan: %+v", result.Plan)
	}
	if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("read-only setup plan changed requested durable state: %v", err)
	}
}

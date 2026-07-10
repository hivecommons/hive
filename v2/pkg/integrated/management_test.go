package integrated

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestUpgradeSameImmutableRefIsIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	ref := "0123456789012345678901234567890123456789"
	if err := store.Save(Config{Repository: "owner/repo", VisualHiveRef: ref, ACMMLevel: 5, Automation: AutomationRepairPR}); err != nil {
		t.Fatal(err)
	}
	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUpgrade, StateDir: stateDir, VisualHiveRef: ref,
		GitHub: hivegithub.NewClientForTest("http://127.0.0.1:1", "owner", []string{"repo"}, slog.Default()),
	})
	if err != nil || !result.Idempotent || result.PRURL != "" {
		t.Fatalf("idempotent upgrade = %+v, %v", result, err)
	}
}

func TestDeleteManagedStateRequiresMarkerAndRemovesOnlyNamedRoot(t *testing.T) {
	unsafe := t.TempDir()
	if err := deleteManagedState(unsafe); err == nil {
		t.Fatal("state deletion without a Hive marker must be rejected")
	}
	managed := t.TempDir()
	writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1"}`)
	if err := deleteManagedState(managed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed state root still exists: %v", err)
	}
}

func TestEnsureCheckoutAlwaysReturnsToRemoteDefault(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runIntegratedGit(t, root, "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "main")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	checkout := filepath.Join(root, "checkout")
	runIntegratedGit(t, root, "clone", remote, checkout)
	runIntegratedGit(t, checkout, "switch", "-c", "hive/setup")
	writeFixture(t, checkout, "setup-only.txt", "dirty branch")
	branch, err := ensureCheckout(context.Background(), "owner/repo", checkout)
	if err != nil || branch != "main" {
		t.Fatalf("ensureCheckout = %q, %v", branch, err)
	}
	head := integratedGitOutput(t, checkout, "rev-parse", "HEAD")
	remoteHead := integratedGitOutput(t, checkout, "rev-parse", "origin/main")
	if head != remoteHead {
		t.Fatalf("managed checkout stayed on a stale branch: head=%s remote=%s", head, remoteHead)
	}
}

func TestGateChecksRequireTerminalVisualVerdictAndRequiredSuccess(t *testing.T) {
	green := hivegithub.PullRequestGate{VisualHiveVerdictGreen: true, RequiredCheckStates: []string{"success"}}
	if !gateChecksGreen(green) {
		t.Fatal("green exact-head gate was rejected")
	}
	green.RequiredCheckStates = []string{"success", "neutral"}
	if gateChecksGreen(green) {
		t.Fatal("neutral required check was accepted")
	}
}

func runIntegratedGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func integratedGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

package integrated

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestManagedUpgradeAndRollbackBlockSetupBaselineRebind(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	head, now := strings.Repeat("a", 40), time.Now().UTC()
	emptyDigest, err := setupBaselineCandidateDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	source := SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		AuthorizerID: 42, SetupPRNumber: 7, SetupPRURL: "https://example.test/pull/7", SetupHeadSHA: head,
		VisualHiveConfigDigest: strings.Repeat("b", 64), ScreenshotContractDigest: strings.Repeat("c", 64), InitialBaselineDigest: emptyDigest,
		CreatedAt: now, UpdatedAt: now,
	}
	target := Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), Coverage: CoverageComprehensive, Automation: AutomationRepairPR}
	targetDigest, err := setupBaselineRebindTargetDigest(target)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := newSetupBaselineAuditReceipt("setup_baseline_rebind_resources_retired", true, "proof")
	if err != nil {
		t.Fatal(err)
	}
	rebind := SetupBaselineRebindIntent{
		SchemaVersion: SetupBaselineRebindSchema, Phase: SetupBaselineRebindResourcesRetired, Repository: "owner/repo", RepositoryID: "123",
		Source: source, TargetConfig: target, TargetConfigDigest: targetDigest, CreatedAt: now, UpdatedAt: now, ResourcesRetiredAt: now, PendingAudit: &receipt,
	}
	if err := store.SaveSetupBaselineRebindIntent(rebind); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []ManagementOperation{OperationUpgrade, OperationRollback} {
		if err := blockPendingSetupBaselineManagement(store, operation); err == nil || !strings.Contains(err.Error(), "rebind") {
			t.Fatalf("%s did not block pending rebind audit: %v", operation, err)
		}
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), setupBaselineRebindFile), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := blockPendingSetupBaselineManagement(store, OperationUpgrade); err == nil || !strings.Contains(err.Error(), "read setup baseline rebind") {
		t.Fatalf("malformed rebind did not block upgrade: %v", err)
	}
}

func TestUpgradeSameImmutableRefIsIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	ref := "0123456789012345678901234567890123456789"
	config := Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationRepairPR, Provider: "codex", ACMMLevel: 5,
		ExecutionMode:   ExecutionLocal,
		MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "owner/visual", VisualHiveRef: ref,
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	server := installedSetupTestServer(t, config)
	defer server.Close()
	result, err := RunManagement(context.Background(), ManagementOptions{
		Operation: OperationUpgrade, StateDir: stateDir, VisualHiveRef: ref,
		GitHub: hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default()),
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
	writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, managed, stateOwnershipMarkerFile, `{"schema_version":"hive.state-owner.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, managed, "integrated/"+repairRetirementFile, `{"known_managed_state":"already_verified_quiescent"}`)
	if err := deleteManagedState(managed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed state root still exists: %v", err)
	}
}

func TestDeleteManagedStateRejectsUnrelatedIntegratedBytes(t *testing.T) {
	managed := t.TempDir()
	writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, managed, stateOwnershipMarkerFile, `{"schema_version":"hive.state-owner.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, managed, "integrated/operator-notes.txt", "must survive")
	if err := deleteManagedState(managed); err == nil || !strings.Contains(err.Error(), "unrelated integrated state entry") {
		t.Fatalf("unrelated integrated bytes were accepted for deletion: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(managed, "integrated", "operator-notes.txt")); err != nil || string(data) != "must survive" {
		t.Fatalf("unrelated integrated sentinel changed: %q err=%v", data, err)
	}
}

func TestDeleteManagedStateRequiresOwnershipMarkerEvenForLegacyConfig(t *testing.T) {
	managed := t.TempDir()
	writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
	if err := deleteManagedState(managed); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("legacy config authorized direct deletion without migration: %v", err)
	}
	if _, err := os.Stat(managed); err != nil {
		t.Fatalf("legacy state was changed: %v", err)
	}
}

func TestDeleteManagedStateAcceptsExactStoppedDaemonFiles(t *testing.T) {
	managed := newManagedDeletionFixture(t)
	writeStoppedDaemonDeletionFixture(t, managed, "owner/repo")
	if err := deleteManagedState(managed); err != nil {
		t.Fatalf("exact stopped daemon state blocked managed deletion: %v", err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("managed state root still exists: %v", err)
	}
}

func TestDeleteManagedStateRejectsUnsafeDaemonFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "malformed status", mutate: func(t *testing.T, root string) {
			writeFixture(t, root, "integrated/daemon.json", `{`)
		}},
		{name: "wrong repository", mutate: func(t *testing.T, root string) {
			writeDaemonDeletionStatus(t, root, "other/repo", root)
		}},
		{name: "wrong state", mutate: func(t *testing.T, root string) {
			writeDaemonDeletionStatus(t, root, "owner/repo", filepath.Join(t.TempDir(), "other-state"))
		}},
		{name: "active status", mutate: func(t *testing.T, root string) {
			status := stoppedDaemonDeletionStatus(root, "owner/repo")
			status["running"] = true
			writeJSONDeletionFixture(t, root, "integrated/daemon.json", status)
		}},
		{name: "malformed lease", mutate: func(t *testing.T, root string) {
			writeFixture(t, root, "integrated/daemon.lease", `{"schema_version":"wrong"}`)
		}},
		{name: "oversized log", mutate: func(t *testing.T, root string) {
			path := filepath.Join(root, "integrated", "daemon.log")
			if err := os.Truncate(path, managedDaemonLogMaxBytes+1); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			managed := newManagedDeletionFixture(t)
			writeStoppedDaemonDeletionFixture(t, managed, "owner/repo")
			test.mutate(t, managed)
			if err := deleteManagedState(managed); err == nil {
				t.Fatal("unsafe daemon state was recursively deleted")
			}
			if _, err := os.Stat(managed); err != nil {
				t.Fatalf("refused daemon fixture was changed: %v", err)
			}
		})
	}
}

func newManagedDeletionFixture(t *testing.T) string {
	t.Helper()
	managed := t.TempDir()
	writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, managed, stateOwnershipMarkerFile, `{"schema_version":"hive.state-owner.v1","repository":"owner/repo","repository_id":"123"}`)
	return managed
}

func writeStoppedDaemonDeletionFixture(t *testing.T, root, repository string) {
	t.Helper()
	writeDaemonDeletionStatus(t, root, repository, root)
	writeJSONDeletionFixture(t, root, "integrated/daemon.lease", map[string]any{
		"schema_version":    "hive.integrated-daemon-lease.v2",
		"pid":               53036,
		"executable":        filepath.Join(root, "hive.exe"),
		"hive_commit":       strings.Repeat("a", 40),
		"executable_sha256": strings.Repeat("b", 64),
		"acquired_at":       "2026-07-12T09:53:01.9417791Z",
	})
	writeFixture(t, root, "integrated/daemon.log", "stopped scheduler output\n")
}

func writeDaemonDeletionStatus(t *testing.T, root, repository, stateDir string) {
	t.Helper()
	writeJSONDeletionFixture(t, root, "integrated/daemon.json", stoppedDaemonDeletionStatus(stateDir, repository))
}

func stoppedDaemonDeletionStatus(stateDir, repository string) map[string]any {
	return map[string]any{
		"schema_version":    "hive.integrated-daemon.v2",
		"pid":               53036,
		"running":           false,
		"repository":        repository,
		"state_dir":         stateDir,
		"executable":        filepath.Join(stateDir, "hive.exe"),
		"hive_commit":       strings.Repeat("a", 40),
		"executable_sha256": strings.Repeat("b", 64),
		"started_at":        "2026-07-12T09:53:01.9507294Z",
		"stopped_at":        "2026-07-12T12:43:50.0679317Z",
		"interval_seconds":  60,
		"last_error":        "scheduler stopped before uninstall",
	}
}

func writeJSONDeletionFixture(t *testing.T, root, relative string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root, relative, string(data))
}

func TestDeleteManagedStateRejectsNestedUnownedCheckoutAndLinkedTree(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "nested unowned checkout", prepare: func(t *testing.T, root string) {
			writeFixture(t, root, "integrated/checkouts/operator/sentinel.txt", "must survive")
		}},
		{name: "linked runtime tree", prepare: func(t *testing.T, root string) {
			outside := t.TempDir()
			writeFixture(t, outside, "sentinel.txt", "must survive")
			if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "runtime", "linked")); err != nil {
				t.Skipf("filesystem link creation unavailable: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			managed := t.TempDir()
			writeFixture(t, managed, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
			writeFixture(t, managed, stateOwnershipMarkerFile, `{"schema_version":"hive.state-owner.v1","repository":"owner/repo","repository_id":"123"}`)
			test.prepare(t, managed)
			if err := deleteManagedState(managed); err == nil {
				t.Fatal("unsafe nested state tree was recursively deleted")
			}
			if _, err := os.Stat(managed); err != nil {
				t.Fatalf("unsafe root was changed: %v", err)
			}
		})
	}
}

func TestDeleteManagedStatePreservesUnrelatedProjectBytes(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	writeFixture(t, project, "integrated/config.json", `{"schema_version":"hive.integrated-config.v1","repository":"owner/repo","repository_id":"123"}`)
	writeFixture(t, project, "src/sentinel.txt", "must survive")
	if err := deleteManagedState(project); err == nil {
		t.Fatal("project directory with unrelated bytes was recursively deleted")
	}
	data, err := os.ReadFile(filepath.Join(project, "src", "sentinel.txt"))
	if err != nil || string(data) != "must survive" {
		t.Fatalf("unrelated project sentinel changed: %q err=%v", data, err)
	}
}

func TestEnsureCheckoutAlwaysReturnsToRemoteDefault(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	bindTestRepositoryCloneURL(t, remote)
	runIntegratedGit(t, root, "init", "--bare", remote)
	seed := filepath.Join(root, "seed")
	runIntegratedGit(t, root, "init", "-b", "main", seed)
	writeFixture(t, seed, "README.md", "main")
	runIntegratedGit(t, seed, "add", ".")
	runIntegratedGit(t, seed, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "seed")
	runIntegratedGit(t, seed, "remote", "add", "origin", remote)
	runIntegratedGit(t, seed, "push", "-u", "origin", "main")
	runIntegratedGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	checkout := filepath.Join(root, "state", "integrated", "checkouts", "owner--repo")
	if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, root, "clone", remote, checkout)
	if err := writeManagedCheckoutOwner(checkout, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	runIntegratedGit(t, checkout, "switch", "-c", "hive/setup")
	writeFixture(t, checkout, "setup-only.txt", "dirty branch")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(checkout, "stale-untracked-link")); err != nil {
		t.Logf("symlink recovery subcheck skipped: %v", err)
	}
	branch, err := ensureCheckout(context.Background(), "owner/repo", checkout)
	if err != nil || branch != "main" {
		t.Fatalf("ensureCheckout = %q, %v", branch, err)
	}
	head := integratedGitOutput(t, checkout, "rev-parse", "HEAD")
	remoteHead := integratedGitOutput(t, checkout, "rev-parse", "origin/main")
	if head != remoteHead {
		t.Fatalf("managed checkout stayed on a stale branch: head=%s remote=%s", head, remoteHead)
	}
	if _, err := os.Lstat(filepath.Join(checkout, "stale-untracked-link")); !os.IsNotExist(err) {
		t.Fatalf("stale untracked link survived managed cleanup: %v", err)
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

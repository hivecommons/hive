package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hostedRuntimeConfigFixture(t *testing.T, root string, release HostedReleaseIdentity) Config {
	t.Helper()
	config := Config{
		Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main",
		Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", ProviderCommand: "codex",
		ExecutionMode: ExecutionHosted, RunIntervalSeconds: 900, HostedSchedule: "*/15 * * * *", HostedStateBranch: "hive/state-42",
		HiveReleaseRepository: "DavidDiaz0317/hive", HiveReleaseVersion: release.Version, HiveCommit: release.HiveCommit,
		DistributionManifestSHA256: release.DistributionManifestSHA256,
		HostedControllerProtocol:   release.HostedControllerProtocol,
		ACMMLevel:                  3, MaxActiveIssues: 5, MaxRepairAttempts: 4, VisualHive: true,
		VisualHiveRepo: "DavidDiaz0317/visual-hive", VisualHiveRef: release.VisualHiveCommit, VisualHiveConfigDigest: strings.Repeat("d", 64),
		TestCommands: [][]string{{"npm", "test"}}, AllowedRepairPaths: []string{"tests/**"},
		AllowedAutoMergePaths: []string{"tests/**"}, CheckoutDir: filepath.Join(root, "old-checkout"), StateDir: root,
		Paused: false, SetupAuthorizationActorID: 99, SetupAuthorizationActorLogin: "Owner",
		SetupAuthorizationWriterID: 202, SetupAuthorizationWriterLogin: "hive-test[bot]", SetupAuthorizationWriterType: "Bot",
		SetupAuthorizationAppID: 303, SetupAuthorizationInstallationID: 404,
		SetupAuthorizationPermissionDigest: strings.Repeat("5", 64), SetupAuthorizationAppBindingDigest: strings.Repeat("6", 64),
		InstalledAt: time.Now().UTC(),
	}
	return bindHostedWorkflowDigest(t, config)
}

func bindHostedWorkflowDigest(t *testing.T, config Config) Config {
	t.Helper()
	workflow, err := GenerateHostedControllerWorkflow(hostedWorkflowConfig(config))
	if err != nil {
		t.Fatalf("generate hosted workflow fixture: %v", err)
	}
	digest := sha256.Sum256([]byte(workflow))
	config.HostedWorkflowSHA256 = hex.EncodeToString(digest[:])
	return config
}

func TestPrepareHostedRuntimeTransitionsOnlyFromExactManagedPredecessor(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := HostedReleaseIdentity{Version: "v0.4.1-integrated.18", HiveCommit: strings.Repeat("a", 40), VisualHiveCommit: strings.Repeat("b", 40), DistributionManifestSHA256: strings.Repeat("c", 64), HostedControllerProtocol: HostedControllerProtocol}
	current := HostedReleaseIdentity{Version: "v0.4.1-integrated.19", HiveCommit: strings.Repeat("d", 40), VisualHiveCommit: strings.Repeat("e", 40), DistributionManifestSHA256: strings.Repeat("f", 64), HostedControllerProtocol: HostedControllerProtocol}
	durable := hostedRuntimeConfigFixture(t, stateDir, previous)
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(durable); err != nil {
		t.Fatal(err)
	}
	candidate := hostedRuntimeConfigFixture(t, stateDir, current)
	candidate.PreviousHostedRelease = &previous
	candidate = bindHostedWorkflowDigest(t, candidate)
	if err := writeManagedFiles(checkout, candidate, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "", nil, false, current, &previous)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.HiveReleaseVersion != current.Version || prepared.HiveCommit != current.HiveCommit || prepared.VisualHiveRef != current.VisualHiveCommit || prepared.StateDir != filepath.Clean(stateDir) || prepared.CheckoutDir != filepath.Clean(checkout) {
		t.Fatalf("hosted runtime did not converge to current release and ephemeral paths: %+v", prepared)
	}
	if prepared.PreviousHostedRelease == nil || !equalHostedReleaseIdentity(*prepared.PreviousHostedRelease, previous) {
		t.Fatalf("hosted runtime lost its exact predecessor binding: %+v", prepared.PreviousHostedRelease)
	}
	if prepared.SetupAuthorizationActorLogin != candidate.SetupAuthorizationActorLogin ||
		prepared.SetupAuthorizationWriterID != candidate.SetupAuthorizationWriterID ||
		prepared.SetupAuthorizationWriterLogin != candidate.SetupAuthorizationWriterLogin ||
		prepared.SetupAuthorizationWriterType != candidate.SetupAuthorizationWriterType ||
		prepared.SetupAuthorizationAppID != candidate.SetupAuthorizationAppID ||
		prepared.SetupAuthorizationInstallationID != candidate.SetupAuthorizationInstallationID ||
		prepared.SetupAuthorizationPermissionDigest != candidate.SetupAuthorizationPermissionDigest ||
		prepared.SetupAuthorizationAppBindingDigest != candidate.SetupAuthorizationAppBindingDigest {
		t.Fatalf("hosted runtime lost the operator/App writer binding: %+v", prepared)
	}

	if err := store.Save(durable); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "", nil, false, current, nil); err == nil {
		t.Fatal("predecessor state was accepted without the exact transition identity")
	}
}

func TestPrepareHostedRuntimeRejectsTamperedManagedController(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	current := HostedReleaseIdentity{Version: "v0.4.1-integrated.19", HiveCommit: strings.Repeat("d", 40), VisualHiveCommit: strings.Repeat("e", 40), DistributionManifestSHA256: strings.Repeat("f", 64), HostedControllerProtocol: HostedControllerProtocol}
	config := hostedRuntimeConfigFixture(t, stateDir, current)
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedFiles(checkout, config, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(checkout, filepath.FromSlash(HostedControllerWorkflowPath))
	if err := os.WriteFile(workflow, []byte("name: tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "", nil, false, current, nil); err == nil {
		t.Fatal("tampered controller workflow was accepted")
	}
}

func TestPrepareHostedRuntimeRequiresProviderOnlyForRepairExecution(t *testing.T) {
	root := t.TempDir()
	stateDir, checkout := filepath.Join(root, "state"), filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	current := HostedReleaseIdentity{Version: "v0.4.1-integrated.19", HiveCommit: strings.Repeat("d", 40), VisualHiveCommit: strings.Repeat("e", 40), DistributionManifestSHA256: strings.Repeat("f", 64), HostedControllerProtocol: HostedControllerProtocol}
	config := hostedRuntimeConfigFixture(t, stateDir, current)
	config.Automation, config.ACMMLevel = AutomationRepairPR, 5
	config = bindHostedWorkflowDigest(t, config)
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedFiles(checkout, config, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "", nil, false, current, nil)
	if err != nil {
		t.Fatalf("control-only runtime required provider: %v", err)
	}
	if prepared.ProviderCommand != "" || len(prepared.ProviderArgs) != 0 {
		t.Fatalf("control-only runtime retained stale provider binding: %+v", prepared.ProviderArgs)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "", nil, true, current, nil); err == nil {
		t.Fatal("repair execution accepted a missing provider")
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	prepared, err = PrepareHostedRuntime(context.Background(), stateDir, checkout, "/release/node", []string{"/release/visual-hive.mjs"}, "/release/codex", nil, true, current, nil)
	if err != nil || prepared.ProviderCommand != "/release/codex" {
		t.Fatalf("repair execution did not bind provider: command=%q err=%v", prepared.ProviderCommand, err)
	}
}

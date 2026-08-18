package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
)

func TestRunDashboardIntegratedPreflightBindsPersistentRuntimeAndModel(t *testing.T) {
	parent := useTemporaryHostedHiveState(t)
	t.Setenv("HIVE_STATE_DIR", filepath.Join(parent, "integrated", "repos", "owner-repository"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/owner/repository" {
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 123, "full_name": "owner/repository", "default_branch": "main"})
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repository"}, slog.Default())
	priorClient, priorProvider := dashboardPreflightGitHubClient, dashboardPreflightResolveProvider
	priorVerify, priorVisual, priorVisualVerify := dashboardPreflightVerifyProvider, dashboardPreflightResolveVisual, dashboardPreflightVerifyVisual
	priorRuntimeIdentity := dashboardPreflightResolveRuntimeIdentity
	dashboardPreflightGitHubClient = func(string) *hivegithub.Client { return client }
	dashboardPreflightResolveProvider = func(context.Context, string, string, []string) (string, error) { return "/release/codex", nil }
	dashboardPreflightVerifyProvider = func(context.Context, string, []string) (string, error) { return "gpt-5.6-sol", nil }
	dashboardPreflightResolveVisual = func(string, []string, string, string) (string, []string, string, error) {
		return "/release/node", []string{"/release/visual-hive.mjs"}, strings.Repeat("a", 40), nil
	}
	dashboardPreflightVerifyVisual = func(context.Context, *hivegithub.Client, string, string) error { return nil }
	dashboardPreflightResolveRuntimeIdentity = func(string, string, []string) (dashboardPreflightRuntimeIdentity, error) {
		return dashboardPreflightRuntimeIdentity{
			HiveCommit: strings.Repeat("1", 40), HiveExecutableSHA256: strings.Repeat("2", 64),
			ProviderBinarySHA256: strings.Repeat("3", 64), VisualCommandSHA256: strings.Repeat("4", 64),
			VisualEntrypointSHA256: strings.Repeat("5", 64), ImageDigest: "sha256:" + strings.Repeat("6", 64),
		}, nil
	}
	t.Cleanup(func() {
		dashboardPreflightGitHubClient, dashboardPreflightResolveProvider = priorClient, priorProvider
		dashboardPreflightVerifyProvider, dashboardPreflightResolveVisual, dashboardPreflightVerifyVisual = priorVerify, priorVisual, priorVisualVerify
		dashboardPreflightResolveRuntimeIdentity = priorRuntimeIdentity
	})
	issues := 3
	result, err := runDashboardIntegratedPreflight(context.Background(), dashboard.IntegratedPreflightRequest{
		Repository: "owner/repository", RequestID: "preflight-cycle-a-001", Provider: "codex", VisualHiveRef: strings.Repeat("a", 40),
		Coverage: "comprehensive", Automation: "repair-pr", MaxActiveIssues: &issues,
	}, "saved-token")
	if err != nil {
		t.Fatalf("hosted integrated preflight: %v", err)
	}
	provider, _ := result["provider"].(map[string]any)
	if result["ready"] != true || result["repository_id"] != "123" || provider["model"] != "gpt-5.6-sol" || provider["model_calls"] != 1 {
		t.Fatalf("unexpected hosted preflight result: %+v", result)
	}
	receipt, exists, err := loadDashboardPreflightReceipt("owner/repository")
	expectedState, stateErr := dashboardSetupStateDir("owner/repository")
	if stateErr != nil || err != nil || !exists || receipt.ProviderModel != "gpt-5.6-sol" || receipt.StateRoot != expectedState ||
		receipt.HiveCommit != strings.Repeat("1", 40) || receipt.ImageDigest != "sha256:"+strings.Repeat("6", 64) {
		t.Fatalf("preflight receipt=%+v exists=%t err=%v", receipt, exists, err)
	}
	entries, err := os.ReadDir(expectedState)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("hosted preflight contaminated fresh repository state: %+v", entries)
	}
	receiptPath, err := dashboardPreflightReceiptPath(integratedStateRoot(), "owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	relativeToState, relativeErr := filepath.Rel(expectedState, receiptPath)
	if relativeErr == nil && relativeToState != ".." && !strings.HasPrefix(relativeToState, ".."+string(filepath.Separator)) && !filepath.IsAbs(relativeToState) {
		t.Fatalf("hosted preflight receipt remained inside repository state: %q", receiptPath)
	}
}

func TestHostedPreflightReceiptBindsSetupAndExpires(t *testing.T) {
	useTemporaryHostedHiveState(t)
	if err := probeIntegratedStateStorage(integratedStateRoot()); err != nil {
		t.Fatal(err)
	}
	issues := 3
	request := dashboard.IntegratedPreflightRequest{
		Repository: "owner/repository", RequestID: "preflight-cycle-a-001", Provider: "codex", VisualHiveRef: strings.Repeat("a", 40),
		Coverage: "comprehensive", Automation: "repair-pr", MaxActiveIssues: &issues,
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	stateDir, err := dashboardSetupStateDir(request.Repository)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := integrated.NormalizeNormalHiveProvider(integrated.SetupOptions{
		Provider: request.Provider, ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.Automation(request.Automation),
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := dashboardPreflightReceipt{
		SchemaVersion: "hive.dashboard-integrated-preflight-receipt.v3", Repository: request.Repository, RepositoryID: "123",
		StateRoot: stateDir, VisualHiveRef: request.VisualHiveRef, Provider: request.Provider, ProviderBinary: "/release/codex",
		ProviderArgs: append([]string(nil), normalized.ProviderArgs...), ProviderModel: "gpt-5.6-sol",
		VisualCommand: "/release/node", VisualArgs: []string{"/release/visual-hive.mjs"},
		HiveCommit: strings.Repeat("1", 40), HiveExecutableSHA256: strings.Repeat("2", 64), ProviderBinarySHA256: strings.Repeat("3", 64),
		VisualCommandSHA256: strings.Repeat("4", 64), VisualEntrypointSHA256: strings.Repeat("5", 64), ImageDigest: "sha256:" + strings.Repeat("6", 64),
		OperatorID: 1, OperatorLogin: "test-owner", WriterID: 1, WriterLogin: "test-owner", WriterType: "User",
		RuntimeBindingDigest: strings.Repeat("0", 64),
		VisualWriterID:       1, VisualWriterLogin: "test-owner", VisualWriterType: "User", VisualBindingDigest: strings.Repeat("0", 64),
		QualityProbe: agent.QualityRuntimeProbeResult{
			Agent: "quality", UID: 2006, Home: "/data/home", CodexHome: "/data/.codex-quality", Backend: "codex",
			Model: "gpt-5.6-sol", CommandSHA256: strings.Repeat("7", 64), ApprovalPolicy: "never",
			ToolCall: "read-only-local-file", OutputSHA256: strings.Repeat("8", 64),
		},
		TestedAt: now, ExpiresAt: now.Add(dashboardPreflightValidity),
	}
	receipt.BindingSHA256, err = dashboardPreflightBinding(request, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), receipt); err != nil {
		t.Fatal(err)
	}
	priorNow := dashboardPreflightNow
	priorProvider, priorVisual, priorRuntime := dashboardPreflightResolveProvider, dashboardPreflightResolveVisual, dashboardPreflightResolveRuntimeIdentity
	dashboardPreflightNow = func() time.Time { return now.Add(time.Minute) }
	dashboardPreflightResolveProvider = func(context.Context, string, string, []string) (string, error) { return receipt.ProviderBinary, nil }
	dashboardPreflightResolveVisual = func(string, []string, string, string) (string, []string, string, error) {
		return receipt.VisualCommand, append([]string(nil), receipt.VisualArgs...), receipt.VisualHiveRef, nil
	}
	dashboardPreflightResolveRuntimeIdentity = func(string, string, []string) (dashboardPreflightRuntimeIdentity, error) {
		return dashboardPreflightRuntimeIdentity{
			HiveCommit: receipt.HiveCommit, HiveExecutableSHA256: receipt.HiveExecutableSHA256, ImageDigest: receipt.ImageDigest,
			ProviderBinarySHA256: receipt.ProviderBinarySHA256, VisualCommandSHA256: receipt.VisualCommandSHA256,
			VisualEntrypointSHA256: receipt.VisualEntrypointSHA256,
		}, nil
	}
	t.Cleanup(func() {
		dashboardPreflightNow, dashboardPreflightResolveProvider, dashboardPreflightResolveVisual, dashboardPreflightResolveRuntimeIdentity = priorNow, priorProvider, priorVisual, priorRuntime
	})
	setup := dashboard.IntegratedSetupRequest{
		Repository: request.Repository, RequestID: "setup-cycle-a-001", Provider: request.Provider, VisualHiveRef: request.VisualHiveRef,
		Coverage: request.Coverage, Automation: request.Automation, MaxActiveIssues: &issues,
	}
	if err := requireDashboardPreflightReceipt(context.Background(), setup); err != nil {
		t.Fatalf("matching hosted preflight receipt was rejected: %v", err)
	}

	coreStore := &liveGitHubRuntimeStore{}
	core, err := coreStore.Publish(testLiveAppRuntimeSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	visualStore := &liveGitHubRuntimeStore{}
	visualCandidate := testDedicatedVisualAppRuntimeSnapshot(core)
	visualCandidate.ExpiresAt = time.Now().UTC().Add(2 * time.Hour)
	visual, err := visualStore.Publish(visualCandidate)
	if err != nil {
		t.Fatal(err)
	}
	brokerReceipt := receipt
	brokerReceipt.WriterID, brokerReceipt.WriterLogin, brokerReceipt.WriterType = core.Writer.ID, core.Writer.Login, core.Writer.Type
	brokerReceipt.AppID, brokerReceipt.InstallationID = core.App.AppID, core.App.InstallationID
	brokerReceipt.PermissionDigest, brokerReceipt.Permissions, brokerReceipt.RuntimeBindingDigest = core.App.PermissionDigest, core.App.Permissions, core.BindingDigest
	brokerReceipt.VisualWriterID, brokerReceipt.VisualWriterLogin, brokerReceipt.VisualWriterType = visual.Writer.ID, visual.Writer.Login, visual.Writer.Type
	brokerReceipt.VisualAppID, brokerReceipt.VisualInstallationID = visual.App.AppID, visual.App.InstallationID
	brokerReceipt.VisualPermissionDigest, brokerReceipt.VisualPermissions = visual.App.PermissionDigest, visual.App.Permissions
	brokerReceipt.VisualBindingDigest, brokerReceipt.VisualTokenExpiresAt = visual.BindingDigest, visual.ExpiresAt
	brokerReceipt.BindingSHA256, err = dashboardPreflightBinding(request, brokerReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), brokerReceipt); err != nil {
		t.Fatal(err)
	}
	renewed := visual
	renewed.ExpiresAt = visual.ExpiresAt.Add(10 * time.Minute)
	operator := hivegithub.AuthenticatedUserIdentity{ID: 1, Login: "test-owner", Type: "User"}
	if err := requireDashboardPreflightReceipt(context.Background(), setup, operator, core, renewed); err != nil {
		t.Fatalf("same brokered App with a renewed lease was rejected: %v", err)
	}
	shorter := visual
	shorter.ExpiresAt = visual.ExpiresAt.Add(-time.Minute)
	if err := requireDashboardPreflightReceipt(context.Background(), setup, operator, core, shorter); err == nil || !strings.Contains(err.Error(), "different setup inputs") {
		t.Fatalf("brokered token lifetime rollback was accepted: %v", err)
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), receipt); err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	tampered.QualityProbe.ApprovalPolicy = "on-request"
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), tampered); err != nil {
		t.Fatal(err)
	}
	if err := requireDashboardPreflightReceipt(context.Background(), setup); err == nil {
		t.Fatal("tampered unattended approval policy was accepted")
	}
	if err := saveDashboardPreflightReceipt(integratedStateRoot(), receipt); err != nil {
		t.Fatal(err)
	}
	setup.VisualHiveRef = strings.Repeat("b", 40)
	if err := requireDashboardPreflightReceipt(context.Background(), setup); err == nil || !strings.Contains(err.Error(), "different setup inputs") {
		t.Fatalf("changed setup binding was not rejected: %v", err)
	}
	setup.VisualHiveRef = request.VisualHiveRef
	dashboardPreflightResolveRuntimeIdentity = func(string, string, []string) (dashboardPreflightRuntimeIdentity, error) {
		return dashboardPreflightRuntimeIdentity{HiveCommit: strings.Repeat("9", 40)}, nil
	}
	if err := requireDashboardPreflightReceipt(context.Background(), setup); err == nil || !strings.Contains(err.Error(), "runtime identity changed") {
		t.Fatalf("changed hosted runtime identity was not rejected: %v", err)
	}
	dashboardPreflightResolveRuntimeIdentity = func(string, string, []string) (dashboardPreflightRuntimeIdentity, error) {
		return dashboardPreflightRuntimeIdentity{
			HiveCommit: receipt.HiveCommit, HiveExecutableSHA256: receipt.HiveExecutableSHA256, ImageDigest: receipt.ImageDigest,
			ProviderBinarySHA256: receipt.ProviderBinarySHA256, VisualCommandSHA256: receipt.VisualCommandSHA256,
			VisualEntrypointSHA256: receipt.VisualEntrypointSHA256,
		}, nil
	}
	dashboardPreflightNow = func() time.Time { return receipt.ExpiresAt.Add(time.Second) }
	if err := requireDashboardPreflightReceipt(context.Background(), setup); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired hosted preflight receipt was not rejected: %v", err)
	}
}

func TestHostedStorageProbeLeavesNoTemporaryFile(t *testing.T) {
	useTemporaryHostedHiveState(t)
	if err := probeIntegratedStateStorage(integratedStateRoot()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(integratedStateRoot())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hive-storage-preflight-") {
			t.Fatalf("storage preflight leaked %s", filepath.Join(integratedStateRoot(), entry.Name()))
		}
	}
}

func TestHostedPreflightReceiptRejectsLinkedDirectoryAndFile(t *testing.T) {
	t.Run("linked directory", func(t *testing.T) {
		useTemporaryHostedHiveState(t)
		path, err := dashboardPreflightReceiptPath(integratedStateRoot(), "owner/repository")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Dir(path)); err != nil {
			if os.IsPermission(err) {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if err := saveDashboardPreflightReceipt(integratedStateRoot(), dashboardPreflightReceipt{Repository: "owner/repository"}); err == nil || !strings.Contains(err.Error(), "must be a real directory") {
			t.Fatalf("linked receipt directory was accepted: %v", err)
		}
	})

	t.Run("linked file", func(t *testing.T) {
		useTemporaryHostedHiveState(t)
		path, err := dashboardPreflightReceiptPath(integratedStateRoot(), "owner/repository")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "receipt.json")
		if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			if os.IsPermission(err) {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if _, _, err := readDashboardPreflightReceiptFile(path); err == nil || !strings.Contains(err.Error(), "regular non-link file") {
			t.Fatalf("linked receipt file was accepted: %v", err)
		}
	})
}

func TestHostedPreflightRejectsExistingControllerLease(t *testing.T) {
	stateDir := t.TempDir()
	leasePath := filepath.Join(stateDir, "integrated", "daemon.lease")
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, []byte("stale-or-live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "owner/repository", false); err == nil || !strings.Contains(err.Error(), "ownership lease already exists") {
		t.Fatalf("existing controller lease was accepted: %v", err)
	}
}

func TestHostedPreflightAllowsExactManagedRuntimeForRebind(t *testing.T) {
	stateDir := t.TempDir()
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	manager, _, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || !active {
		t.Fatalf("activate exact managed runtime: active=%v err=%v", active, err)
	}
	original := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(manager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(original) })
	if _, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "owner/repository", true); err == nil || !strings.Contains(err.Error(), "missing its authoritative ownership lease") {
		t.Fatalf("active runtime without its ownership lease was accepted: %v", err)
	}
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseDaemonLease(lease) })

	active, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "OWNER/REPOSITORY", true)
	if err != nil || !active {
		t.Fatalf("exact managed update runtime rejected: %v", err)
	}
	if _, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "other/repository", true); err == nil {
		t.Fatal("different repository runtime accepted for managed update")
	}
	if _, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "owner/repository", false); err == nil {
		t.Fatal("fresh setup accepted an existing runtime")
	}
}

func TestHostedPreflightAllowsLeaseFreeIntakeRuntimeForManagedAuthorityTransition(t *testing.T) {
	stateDir := t.TempDir()
	installed := testInstalledNormalVisualContract(t, stateDir)
	installed.Automation = integrated.AutomationIssues
	contract := &testNormalVisualContract{config: installed, exists: true}
	manager, _, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	if active, err := manager.Ensure(context.Background()); err != nil || !active {
		t.Fatalf("activate exact managed intake runtime: active=%v err=%v", active, err)
	}
	original := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(manager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(original) })

	active, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "OWNER/REPOSITORY", true)
	if err != nil || !active {
		t.Fatalf("lease-free intake runtime rejected for managed authority transition: active=%v err=%v", active, err)
	}

	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseDaemonLease(lease) })
	if _, err := ensureNoVisualRuntimeBeforeSetup(stateDir, "owner/repository", true); err == nil || !strings.Contains(err.Error(), "intake-only controller unexpectedly holds") {
		t.Fatalf("unexpected intake-only ownership lease was accepted: %v", err)
	}
}

func TestHostedPreflightRejectsManagedContractWithoutDurableState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/owner/repository/contents/.hive/integrated.json" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"type": "file", "path": ".hive/integrated.json", "name": "integrated.json",
			})
			return
		}
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repository"}, slog.Default())
	_, err := ensureManagedRepositoryContractBeforeSetup(context.Background(), client, t.TempDir(), "owner/repository", 123, "owner", "repository", "main")
	if err == nil || !strings.Contains(err.Error(), "setup-reset") {
		t.Fatalf("orphaned managed contract was accepted by hosted preflight: %v", err)
	}
}

func TestHostedPreflightAcceptsExactManagedContractUpdate(t *testing.T) {
	stateRoot := t.TempDir()
	checkout := filepath.Join(stateRoot, "integrated", "checkout")
	contractPath := filepath.Join(checkout, ".hive", "integrated.json")
	if err := os.MkdirAll(filepath.Dir(contractPath), 0o700); err != nil {
		t.Fatal(err)
	}
	contract := []byte("{\"schema_version\":\"hive.integrated.v1\",\"repository_id\":\"123\"}\n")
	if err := os.WriteFile(contractPath, contract, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := integrated.NewStore(filepath.Join(stateRoot, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{
		Repository: "Owner/Repository", RepositoryID: "123", DefaultBranch: "main",
		StateDir: stateRoot, CheckoutDir: checkout, SetupPRNumber: 9,
		SetupHeadSHA: strings.Repeat("a", 40),
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/repos/owner/repository/contents/.hive/integrated.json" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"type": "file", "path": ".hive/integrated.json", "name": "integrated.json",
				"encoding": "base64", "content": base64.StdEncoding.EncodeToString(contract),
			})
			return
		}
		http.Error(writer, `{\"message\":\"not found\"}`, http.StatusNotFound)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repository"}, slog.Default())
	managed, err := ensureManagedRepositoryContractBeforeSetup(
		context.Background(), client, stateRoot, "owner/repository", 123, "owner", "repository", "main",
	)
	if err != nil || !managed {
		t.Fatalf("exact managed contract update rejected: managed=%v err=%v", managed, err)
	}
}

func TestLocalSetupDoesNotRequireHostedPreflightReceipt(t *testing.T) {
	useTemporaryHiveHome(t)
	request := dashboard.IntegratedSetupRequest{Repository: "owner/repository", Provider: "codex", VisualHiveRef: strings.Repeat("a", 40)}
	if err := requireDashboardPreflightReceipt(context.Background(), request); err != nil {
		t.Fatalf("ordinary local setup was changed by hosted preflight: %v", err)
	}
}

func TestNormalizeDashboardPreflightProviderDefaultsModelForIssues(t *testing.T) {
	normalized, err := normalizeDashboardPreflightProvider("codex", integrated.AutomationIssues)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.ProviderArgs) != 1 || normalized.ProviderArgs[0] != "--model=gpt-5.6-sol" {
		t.Fatalf("issues preflight did not bind the audited Codex default: %v", normalized.ProviderArgs)
	}
}

func TestVerifyDashboardPreflightProviderRejectsMissingModelCleanly(t *testing.T) {
	_, err := verifyDashboardPreflightProvider(context.Background(), "/unused/codex", nil)
	if err == nil || !strings.Contains(err.Error(), "configured Codex model is missing") || strings.Contains(err.Error(), "%!w") {
		t.Fatalf("unexpected missing-model diagnostic: %v", err)
	}
}

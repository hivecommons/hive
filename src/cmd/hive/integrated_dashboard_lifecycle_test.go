package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hostedcontrol"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/visualhive/normalservice"
)

func writeDashboardLifecycleContract(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "repository-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{
		Repository: "owner/repository", RepositoryID: "123", StateDir: stateDir,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)
	return stateDir
}

func TestDashboardLifecycleMutationRequiresCompleteAppPermissions(t *testing.T) {
	runtimeSnapshot := testLiveAppRuntimeSnapshot()
	delete(runtimeSnapshot.App.Permissions, "workflows")
	if err := requireDashboardLifecycleMutationPermissions(dashboard.IntegratedLifecycleRequest{Operation: "control-apply"}, runtimeSnapshot); err == nil || !strings.Contains(err.Error(), "workflows") {
		t.Fatalf("mutation accepted an App without Workflows write: %v", err)
	}
	if err := requireDashboardLifecycleMutationPermissions(dashboard.IntegratedLifecycleRequest{Operation: "control-plan"}, runtimeSnapshot); err != nil {
		t.Fatalf("read-only lifecycle plan was incorrectly blocked: %v", err)
	}
	runtimeSnapshot.App.Permissions["workflows"] = "write"
	if err := requireDashboardLifecycleMutationPermissions(dashboard.IntegratedLifecycleRequest{Operation: "baseline-approve"}, runtimeSnapshot); err != nil {
		t.Fatalf("complete App permission set was rejected: %v", err)
	}
}

func installDashboardLifecycleFakes(t *testing.T) {
	t.Helper()
	originalRunner := dashboardLifecycleCLIRunner
	originalTrigger := dashboardLifecycleTrigger
	originalPlanRetirement := dashboardLifecyclePlanRetirement
	originalRetireRepair := dashboardLifecycleRetireRepair
	originalGitHubClient := dashboardLifecycleGitHubClient
	originalStopNormalVisual := dashboardLifecycleStopNormalVisual
	originalNormalBeadsDirs := append([]string(nil), dashboardLifecycleNormalBeadsDirs...)
	t.Cleanup(func() {
		dashboardLifecycleCLIRunner = originalRunner
		dashboardLifecycleTrigger = originalTrigger
		dashboardLifecyclePlanRetirement = originalPlanRetirement
		dashboardLifecycleRetireRepair = originalRetireRepair
		dashboardLifecycleGitHubClient = originalGitHubClient
		dashboardLifecycleStopNormalVisual = originalStopNormalVisual
		dashboardLifecycleNormalBeadsDirs = originalNormalBeadsDirs
	})
}

func TestDashboardUninstalledReadsDoNotCreateRecoveryState(t *testing.T) {
	parent := useTemporaryHostedHiveState(t)
	stateRoot := filepath.Join(parent, "integrated")
	installDashboardLifecycleFakes(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not installed", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	dashboardLifecycleGitHubClient = func(string) *hivegithub.Client {
		return hivegithub.NewClient("owner-token", "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), server.URL)
	}

	var selected string
	for _, operation := range []string{"status", "doctor"} {
		result, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
			Repository: "owner/repository", Operation: operation,
		}, "owner-token")
		if err != nil || result["installed"] != false {
			t.Fatalf("%s result=%v err=%v", operation, result, err)
		}
		got, _ := result["selected_state_dir"].(string)
		if selected == "" {
			selected = got
		} else if got != selected {
			t.Fatalf("uninstalled reads drifted from %q to %q", selected, got)
		}
		if _, statErr := os.Lstat(stateRoot); !os.IsNotExist(statErr) {
			t.Fatalf("%s created read-only lifecycle state: %v", operation, statErr)
		}
	}
}

func TestDashboardRepairRetirementPlanApplyAndReplayBindExactProposal(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	dashboardLifecycleCLIRunner = func(_ context.Context, args []string, _ string, _ bool) (map[string]any, []byte, error) {
		if args[0] != "status" {
			t.Fatalf("unexpected command %v", args)
		}
		return map[string]any{
			"schema_version": "hive.status.v1", "config": map[string]any{"repository": "owner/repository"},
		}, []byte(`{"schema_version":"hive.status.v1"}`), nil
	}
	retirement := normalservice.RepairRetirementPlan{
		SchemaVersion: "hive.normal-visual-repair-retirement.v1", Repository: "owner/repository",
		RepositoryFingerprint: strings.Repeat("a", 64), SourceExternalRef: "visual-hive://owner/repository/finding",
		WorkflowKey: strings.Repeat("b", 64), WorkOrderID: "swo-" + strings.Repeat("c", 64),
		RequestSHA256: strings.Repeat("c", 64), BaseBranch: "main", BaseSHA: strings.Repeat("d", 40),
		CurrentDefaultHeadSHA: strings.Repeat("e", 40), PullRequestNumber: 17,
		PullRequestURL: "https://github.test/owner/repository/pull/17", Branch: "hive/repair-proof",
		HeadSHA: strings.Repeat("f", 40), VerdictReceiptSHA256: strings.Repeat("1", 64),
		RepairRetirementReasonCode: "default_branch_changed_requires_fresh_authoritative_verification",
	}
	planCalls, applyCalls := 0, 0
	dashboardLifecyclePlanRetirement = func(context.Context) (normalservice.RepairRetirementPlan, error) {
		planCalls++
		return retirement, nil
	}
	dashboardLifecycleRetireRepair = func(_ context.Context, expected *normalservice.RepairRetirementPlan) error {
		applyCalls++
		if expected == nil || !reflect.DeepEqual(*expected, retirement) {
			t.Fatalf("retirement apply lost exact plan: %+v", expected)
		}
		return nil
	}
	request := dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-plan", Action: "repair-retire",
		RequestID: "cycle-a-repair-retire-001",
	}
	plan, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := plan["repair_retirement"].(normalservice.RepairRetirementPlan); !ok || !reflect.DeepEqual(got, retirement) {
		t.Fatalf("control plan did not expose exact repair binding: %+v", plan)
	}
	request.Operation = "control-apply"
	request.ExpectedPlanSHA256, _ = plan["plan_sha256"].(string)
	result, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil || result["outcome"] != "retired_for_fresh_verification" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil || replay["idempotent_replay"] != true || applyCalls != 1 || planCalls != 2 {
		t.Fatalf("replay=%+v err=%v planCalls=%d applyCalls=%d", replay, err, planCalls, applyCalls)
	}
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil || len(ledger.Entries) != 1 || ledger.Entries[0].Operation != "repair-retire" {
		t.Fatalf("ledger=%+v err=%v", ledger, err)
	}
}

func TestDashboardIntegratedLifecycleStatusUsesAuthoritativeState(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	var gotArgs []string
	dashboardLifecycleCLIRunner = func(_ context.Context, args []string, token string, allowNonzero bool) (map[string]any, []byte, error) {
		gotArgs = append([]string(nil), args...)
		if token != "owner-token" || allowNonzero {
			t.Fatalf("runner token=%q allowNonzero=%t", token, allowNonzero)
		}
		return map[string]any{"schema_version": "hive.status.v1"}, []byte(`{"schema_version":"hive.status.v1"}`), nil
	}
	result, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "status",
	}, "owner-token")
	if err != nil || result["schema_version"] != "hive.status.v1" {
		t.Fatalf("result=%v err=%v", result, err)
	}
	want := []string{"status", "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("status args=%v want=%v", gotArgs, want)
	}
}

func TestDashboardIntegratedControlPlanApplyAndReplay(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	statusCalls := 0
	pauseCalls := 0
	dashboardLifecycleCLIRunner = func(_ context.Context, args []string, token string, _ bool) (map[string]any, []byte, error) {
		if token != "owner-token" {
			t.Fatalf("runner token=%q", token)
		}
		switch args[0] {
		case "status":
			statusCalls++
			return map[string]any{
				"schema_version": "hive.status.v1",
				"paused":         false,
				"config":         map[string]any{"repository": "owner/repository"},
				"health_message": fmt.Sprintf("last activity %d seconds ago", statusCalls),
			}, []byte(fmt.Sprintf(`{"paused":false,"health_message":"last activity %d seconds ago"}`, statusCalls)), nil
		case "pause":
			pauseCalls++
			return map[string]any{"repository": "owner/repository", "paused": true}, []byte(`{"paused":true}`), nil
		default:
			t.Fatalf("unexpected command %v", args)
			return nil, nil, nil
		}
	}
	base := dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-plan", Action: "pause", RequestID: "cycle-a-pause-001",
	}
	plan, err := runDashboardIntegratedLifecycle(context.Background(), base, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	planSHA, _ := plan["plan_sha256"].(string)
	if len(planSHA) != 64 {
		t.Fatalf("plan=%v", plan)
	}
	base.Operation = "control-apply"
	base.ExpectedPlanSHA256 = planSHA
	first, err := runDashboardIntegratedLifecycle(context.Background(), base, "owner-token")
	if err != nil || first["paused"] != true {
		t.Fatalf("first=%v err=%v", first, err)
	}
	if statusCalls != 2 {
		t.Fatalf("control apply did not re-read stable status: calls=%d", statusCalls)
	}
	replay, err := runDashboardIntegratedLifecycle(context.Background(), base, "owner-token")
	if err != nil || replay["idempotent_replay"] != true || pauseCalls != 1 {
		t.Fatalf("replay=%v err=%v pauseCalls=%d", replay, err, pauseCalls)
	}
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil || len(ledger.Entries) != 1 || ledger.Entries[0].Status != "completed" {
		t.Fatalf("ledger=%+v err=%v", ledger, err)
	}
	paths, err := hostedcontrol.PortableInventory(stateDir)
	if err != nil {
		t.Fatalf("dashboard receipt broke portable state: %v", err)
	}
	for _, path := range paths {
		if path == "integrated/dashboard-control-requests.json" {
			t.Fatalf("operational dashboard receipt leaked into portable repository state: %v", paths)
		}
	}
	ledgerPath, err := dashboardLifecycleLedgerPath(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(ledgerPath); err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("dashboard receipt path is not private: info=%v err=%v", info, err)
	}
}

func TestDashboardUninstallStopsNormalVisualBeforeCLI(t *testing.T) {
	for _, action := range []string{"uninstall", "uninstall-finalize"} {
		t.Run(action, func(t *testing.T) {
			writeDashboardLifecycleContract(t)
			installDashboardLifecycleFakes(t)
			var order []string
			var uninstallArgs []string
			dashboardLifecycleNormalBeadsDirs = []string{"/data/beads/quality", "/data/beads/security"}
			dashboardLifecycleStopNormalVisual = func(context.Context) error {
				order = append(order, "stop-normal-visual")
				return nil
			}
			dashboardLifecycleCLIRunner = func(_ context.Context, args []string, _ string, _ bool) (map[string]any, []byte, error) {
				switch args[0] {
				case "status":
					return map[string]any{
						"schema_version": "hive.status.v1",
						"paused":         true,
						"config":         map[string]any{"repository": "owner/repository"},
					}, []byte(`{"schema_version":"hive.status.v1","paused":true}`), nil
				case "uninstall":
					order = append(order, "uninstall-cli")
					uninstallArgs = append([]string(nil), args...)
					return map[string]any{"state_deleted": action == "uninstall-finalize"}, []byte(`{"state_deleted":true}`), nil
				default:
					t.Fatalf("unexpected command %v", args)
					return nil, nil, nil
				}
			}
			request := dashboard.IntegratedLifecycleRequest{
				Repository: "owner/repository", Operation: "control-plan", Action: action,
				RequestID: "cycle-a-" + action + "-stop-001",
			}
			plan, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
			if err != nil {
				t.Fatal(err)
			}
			request.Operation = "control-apply"
			request.ExpectedPlanSHA256, _ = plan["plan_sha256"].(string)
			result, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
			if err != nil || result == nil || !reflect.DeepEqual(order, []string{"stop-normal-visual", "uninstall-cli"}) {
				t.Fatalf("result=%v order=%v err=%v", result, order, err)
			}
			wantArgs := []string{
				"uninstall", "--state-dir", os.Getenv("HIVE_STATE_DIR"), "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
				"--beads-dir", "/data/beads/quality", "--beads-dir", "/data/beads/security",
			}
			if action == "uninstall-finalize" {
				wantArgs = append(wantArgs, "--delete-state")
			}
			if !reflect.DeepEqual(uninstallArgs, wantArgs) {
				t.Fatalf("ordinary Hive beads directory was not bound to uninstall: %v", uninstallArgs)
			}
		})
	}
}

func TestDashboardIntegratedControlRejectsPlanDrift(t *testing.T) {
	writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	dashboardLifecycleCLIRunner = func(context.Context, []string, string, bool) (map[string]any, []byte, error) {
		return map[string]any{"schema_version": "hive.status.v1"}, []byte(`{"schema_version":"hive.status.v1"}`), nil
	}
	_, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-apply", Action: "trigger",
		RequestID: "cycle-a-trigger-001", ExpectedPlanSHA256: strings.Repeat("0", 64),
	}, "owner-token")
	if err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatalf("plan drift err=%v", err)
	}
}

func TestDashboardIntegratedControlRecoversStaleReceiptWithoutReplanning(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	planSHA := strings.Repeat("a", 64)
	if err := saveDashboardLifecycleLedger(stateDir, dashboardLifecycleLedger{
		SchemaVersion: dashboardLifecycleLedgerSchema,
		Entries: []dashboardLifecycleRequestRecord{{
			RequestID:  "cycle-a-pause-stale-001",
			Operation:  "pause",
			PlanSHA256: planSHA,
			Status:     "started",
			StartedAt:  time.Now().UTC().Add(-56 * time.Minute),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	statusCalls := 0
	pauseCalls := 0
	dashboardLifecycleCLIRunner = func(_ context.Context, args []string, _ string, _ bool) (map[string]any, []byte, error) {
		switch args[0] {
		case "status":
			statusCalls++
			return map[string]any{"paused": true}, []byte(`{"paused":true}`), nil
		case "pause":
			pauseCalls++
			return map[string]any{"paused": true}, []byte(`{"paused":true}`), nil
		default:
			t.Fatalf("unexpected command %v", args)
			return nil, nil, nil
		}
	}
	result, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-apply", Action: "pause",
		RequestID: "cycle-a-pause-stale-001", ExpectedPlanSHA256: planSHA,
	}, "owner-token")
	if err != nil || result["paused"] != true {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if statusCalls != 0 || pauseCalls != 1 {
		t.Fatalf("statusCalls=%d pauseCalls=%d", statusCalls, pauseCalls)
	}
	replay, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-apply", Action: "pause",
		RequestID: "cycle-a-pause-stale-001", ExpectedPlanSHA256: planSHA,
	}, "owner-token")
	if err != nil || replay["idempotent_replay"] != true || pauseCalls != 1 {
		t.Fatalf("replay=%v err=%v pauseCalls=%d", replay, err, pauseCalls)
	}
}

func TestDashboardIntegratedTriggerUsesExistingNormalService(t *testing.T) {
	writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	statusBytes := []byte(`{"schema_version":"hive.status.v1"}`)
	dashboardLifecycleCLIRunner = func(context.Context, []string, string, bool) (map[string]any, []byte, error) {
		return map[string]any{"schema_version": "hive.status.v1"}, statusBytes, nil
	}
	triggerCalls := 0
	dashboardLifecycleTrigger = func(context.Context) error {
		triggerCalls++
		return normalservice.ErrNoDispatch
	}
	request := dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-plan", Action: "trigger", RequestID: "cycle-a-trigger-002",
	}
	plan, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	request.Operation = "control-apply"
	request.ExpectedPlanSHA256 = plan["plan_sha256"].(string)
	result, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil || result["outcome"] != "held" || triggerCalls != 1 {
		t.Fatalf("result=%v err=%v calls=%d", result, err, triggerCalls)
	}
}

func TestDashboardIntegratedTriggerSurvivesCallerCancellation(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	dashboardLifecycleCLIRunner = func(_ context.Context, _ []string, _ string, _ bool) (map[string]any, []byte, error) {
		return map[string]any{
			"schema_version": "hive.status.v1",
			"config":         map[string]any{"repository": "owner/repository"},
		}, []byte(`{"schema_version":"hive.status.v1"}`), nil
	}
	request := dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-plan", Action: "trigger",
		RequestID: "cycle-a-trigger-disconnect-001",
	}
	plan, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	request.Operation = "control-apply"
	request.ExpectedPlanSHA256, _ = plan["plan_sha256"].(string)
	caller, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	dashboardLifecycleTrigger = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("durable trigger inherited caller cancellation: %v", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("durable trigger has no bounded deadline")
		}
		return nil
	}
	result, err := runDashboardIntegratedLifecycle(caller, request, "owner-token")
	if err != nil || result["outcome"] != "completed" {
		t.Fatalf("result=%v err=%v", result, err)
	}
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil || len(ledger.Entries) != 1 || ledger.Entries[0].Status != "completed" {
		t.Fatalf("ledger=%+v err=%v", ledger, err)
	}
}

func TestDashboardIntegratedTriggerTreatsSetupBaselineProgressAsHeld(t *testing.T) {
	writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	statusBytes := []byte(`{"schema_version":"hive.status.v1"}`)
	dashboardLifecycleCLIRunner = func(context.Context, []string, string, bool) (map[string]any, []byte, error) {
		return map[string]any{"schema_version": "hive.status.v1"}, statusBytes, nil
	}
	dashboardLifecycleTrigger = func(context.Context) error {
		return fmt.Errorf("advance baseline: %w", integrated.ErrSetupBaselineLifecycleHold)
	}
	request := dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-plan", Action: "trigger", RequestID: "cycle-a-trigger-baseline-001",
	}
	plan, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	request.Operation = "control-apply"
	request.ExpectedPlanSHA256 = plan["plan_sha256"].(string)
	result, err := runDashboardIntegratedLifecycle(context.Background(), request, "owner-token")
	if err != nil || result["outcome"] != "held" || !strings.Contains(result["detail"].(string), "advance baseline") {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestDashboardBaselineApprovalBindsExactCLIArgumentsAndReceipt(t *testing.T) {
	stateDir := writeDashboardLifecycleContract(t)
	installDashboardLifecycleFakes(t)
	var gotArgs []string
	dashboardLifecycleCLIRunner = func(_ context.Context, args []string, token string, allowNonzero bool) (map[string]any, []byte, error) {
		gotArgs = append([]string(nil), args...)
		return map[string]any{"schema_version": "hive.baseline-approval.v1"}, []byte(`{"schema_version":"hive.baseline-approval.v1"}`), nil
	}
	approval := &dashboard.IntegratedBaselineApprovalRequest{
		RequestID: "baseline-cycle-a-001", RepositoryID: "123", CaptureRunID: 41, ArtifactID: 42, PRNumber: 17,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), DiffDigest: strings.Repeat("c", 64),
		CandidateDigest: strings.Repeat("d", 64), ActorID: 99, PlanDigest: strings.Repeat("e", 64),
		Reason: "Reviewed every exact candidate PNG at original resolution.",
	}
	result, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "baseline-approve", Baseline: approval,
	}, "owner-token")
	if err != nil || result["schema_version"] != "hive.baseline-approval.v1" {
		t.Fatalf("result=%v err=%v", result, err)
	}
	required := []string{
		"approve-baseline", "--state-dir", stateDir, "--repo-id", "123", "--run-id", "41", "--artifact-id", "42",
		"--pr", "17", "--head", approval.HeadSHA, "--base", approval.BaseSHA, "--diff-digest", approval.DiffDigest,
		"--candidate-digest", approval.CandidateDigest, "--actor-id", "99", "--plan-digest", approval.PlanDigest,
		"--reason", approval.Reason, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
	}
	if !reflect.DeepEqual(gotArgs, required) {
		t.Fatalf("baseline args=%v want=%v", gotArgs, required)
	}
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil || len(ledger.Entries) != 1 || ledger.Entries[0].RequestID != approval.RequestID {
		t.Fatalf("ledger=%+v err=%v", ledger, err)
	}
}

func TestDashboardLifecycleMutationFailureIsDurablyBound(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "repository-state")
	sum := sha256.Sum256([]byte("test plan"))
	plan := hex.EncodeToString(sum[:])
	calls := 0
	_, err := runDashboardLifecycleMutation(stateDir, "failure-request-001", "pause", plan, func() (map[string]any, error) {
		calls++
		return nil, errors.New("bounded failure")
	})
	if err == nil {
		t.Fatal("mutation failure was hidden")
	}
	_, replayErr := runDashboardLifecycleMutation(stateDir, "failure-request-001", "pause", plan, func() (map[string]any, error) {
		calls++
		return map[string]any{"unexpected": true}, nil
	})
	if replayErr == nil || calls != 1 || !strings.Contains(replayErr.Error(), "already failed") {
		t.Fatalf("replayErr=%v calls=%d", replayErr, calls)
	}
}

func TestDashboardLifecycleFinalizationReceiptDoesNotRecreateDeletedState(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "repository-state")
	if err := os.MkdirAll(filepath.Join(stateDir, "integrated"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := strings.Repeat("f", 64)
	calls := 0
	ledgerStateDir := dashboardLifecycleLedgerStateDir(stateDir, "uninstall-finalize")
	result, err := runDashboardLifecycleMutation(ledgerStateDir, "uninstall-cycle-a-finalize-001", "uninstall-finalize", plan, func() (map[string]any, error) {
		calls++
		if err := os.RemoveAll(stateDir); err != nil {
			return nil, err
		}
		return map[string]any{"uninstalled": true}, nil
	})
	if err != nil || result["uninstalled"] != true {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("finalization receipt recreated deleted state: err=%v", err)
	}
	replay, err := runDashboardLifecycleMutation(ledgerStateDir, "uninstall-cycle-a-finalize-001", "uninstall-finalize", plan, func() (map[string]any, error) {
		calls++
		return nil, errors.New("replayed finalization")
	})
	if err != nil || replay["idempotent_replay"] != true || calls != 1 {
		t.Fatalf("replay=%v err=%v calls=%d", replay, err, calls)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)
	fullReplay, err := runDashboardIntegratedLifecycle(context.Background(), dashboard.IntegratedLifecycleRequest{
		Repository: "owner/repository", Operation: "control-apply", Action: "uninstall-finalize",
		RequestID: "uninstall-cycle-a-finalize-001", ExpectedPlanSHA256: plan,
	}, "owner-token")
	if err != nil || fullReplay["idempotent_replay"] != true {
		t.Fatalf("full replay after contract deletion=%v err=%v", fullReplay, err)
	}
}

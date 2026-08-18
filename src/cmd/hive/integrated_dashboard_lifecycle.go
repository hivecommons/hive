package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/visualhive/normalservice"
)

const (
	dashboardLifecycleLedgerSchema    = "hive.dashboard-integrated-requests.v1"
	dashboardLifecycleMutationTimeout = 55 * time.Minute
)

var (
	dashboardLifecycleCLIRunner      = runDashboardLifecycleCLI
	dashboardLifecycleTrigger        = triggerDashboardVisualCycle
	dashboardLifecyclePlanRetirement = planDashboardRepairRetirement
	dashboardLifecycleRetireRepair   = retireDashboardRepair
	dashboardLifecycleGitHubClient   = func(token string) *hivegithub.Client {
		return hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	}
	dashboardLifecycleStopNormalVisual func(context.Context) error
	dashboardLifecycleNormalBeadsDirs  []string
	dashboardLifecycleMu               sync.Mutex
)

type dashboardLifecycleLedger struct {
	SchemaVersion string                            `json:"schema_version"`
	Entries       []dashboardLifecycleRequestRecord `json:"entries"`
}

type dashboardLifecycleRequestRecord struct {
	RequestID   string          `json:"request_id"`
	Operation   string          `json:"operation"`
	PlanSHA256  string          `json:"plan_sha256"`
	Status      string          `json:"status"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt time.Time       `json:"completed_at,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorSHA256 string          `json:"error_sha256,omitempty"`
}

func runDashboardIntegratedLifecycle(ctx context.Context, request dashboard.IntegratedLifecycleRequest, credential any) (map[string]any, error) {
	_, legacyTestCredential := credential.(string)
	operator, runtimeSnapshot, token, err := resolveDashboardLifecycleCredential(ctx, request.Repository, credential)
	if err != nil {
		return nil, err
	}
	if runtimeSnapshot.Mode == "app" && (request.Operation == "control-apply" || request.Operation == "baseline-approve") {
		runtimeSnapshot, err = refreshLiveGitHubAppRuntime(ctx, runtimeSnapshot)
		if err != nil {
			return nil, err
		}
	}
	if err := requireDashboardLifecycleMutationPermissions(request, runtimeSnapshot); err != nil {
		return nil, err
	}
	stateDir := ""
	if request.Operation == "status" || request.Operation == "doctor" {
		if _, exists, contractErr := loadAuthoritativeVisualWorkContract(); contractErr == nil && !exists {
			return dashboardIntegratedUninstalledRead(ctx, request, runtimeSnapshot)
		}
	}
	if request.Action == "setup-reset" || request.Action == "setup-reset-finalize" {
		var err error
		stateDir, err = dashboardSetupResetStateDir(request.Repository)
		if err != nil {
			return nil, err
		}
	}
	if request.Operation == "control-apply" {
		candidateStateDir, candidateErr := dashboardSetupStateDir(request.Repository)
		if request.Action == "setup-reset" || request.Action == "setup-reset-finalize" {
			candidateStateDir, candidateErr = dashboardSetupResetStateDir(request.Repository)
		}
		if candidateErr == nil {
			ledgerStateDir := dashboardLifecycleLedgerStateDir(candidateStateDir, request.Action)
			replay, terminal, recoverStale, replayErr := replayDashboardLifecycleMutation(
				ledgerStateDir, request.RequestID, request.Action, request.ExpectedPlanSHA256,
			)
			if replayErr != nil || terminal {
				return replay, replayErr
			}
			if recoverStale {
				stateDir = candidateStateDir
			}
		}
	}
	if stateDir == "" {
		var err error
		stateDir, err = dashboardIntegratedStateDir(request.Repository, request.Action)
		if err != nil {
			return nil, err
		}
	}
	switch request.Operation {
	case "status", "doctor":
		result, _, runErr := dashboardLifecycleCLIRunner(ctx, []string{
			request.Operation, "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
		}, token, request.Operation == "doctor")
		if result != nil {
			augmentDashboardIntegratedRead(result, request.Repository, stateDir)
		}
		return result, runErr
	case "baseline-plan":
		result, runErr := integrated.ApproveSetupBaseline(ctx, integrated.ApproveSetupBaselineOptions{
			StateDir: stateDir, PlanOnly: true, GitHub: runtimeSnapshot.Client, GitTransportToken: token, AuthorizationActor: operator,
		})
		if runErr != nil {
			return nil, runErr
		}
		return dashboardLifecycleResultMap(result)
	case "baseline-approve":
		mutationCtx, cancel := durableDashboardLifecycleContext(ctx)
		defer cancel()
		return runDashboardBaselineApproval(mutationCtx, stateDir, request, operator, runtimeSnapshot, token, legacyTestCredential)
	case "control-plan":
		return dashboardIntegratedControlPlan(ctx, stateDir, request, operator, runtimeSnapshot, token)
	case "control-apply":
		mutationCtx, cancel := durableDashboardLifecycleContext(ctx)
		defer cancel()
		return runDashboardIntegratedControl(mutationCtx, stateDir, request, operator, runtimeSnapshot, token, legacyTestCredential)
	default:
		return nil, fmt.Errorf("unsupported integrated dashboard operation %q", request.Operation)
	}
}

func requireDashboardLifecycleMutationPermissions(request dashboard.IntegratedLifecycleRequest, runtimeSnapshot liveGitHubRuntimeSnapshot) error {
	if runtimeSnapshot.Mode != "app" || (request.Operation != "control-apply" && request.Operation != "baseline-approve") {
		return nil
	}
	if err := runtimeSnapshot.App.RequireVisualHivePermissions(); err != nil {
		return fmt.Errorf("live GitHub App permissions do not permit integrated lifecycle mutation: %w", err)
	}
	return nil
}

func resolveDashboardLifecycleCredential(ctx context.Context, repository string, credential any) (hivegithub.AuthenticatedUserIdentity, liveGitHubRuntimeSnapshot, string, error) {
	switch value := credential.(type) {
	case hivegithub.AuthenticatedUserIdentity:
		runtimeSnapshot, err := currentDashboardGitHubRuntime()
		if err != nil {
			return value, liveGitHubRuntimeSnapshot{}, "", err
		}
		if !strings.EqualFold(runtimeSnapshot.Repository, repository) {
			return value, liveGitHubRuntimeSnapshot{}, "", fmt.Errorf("live GitHub runtime belongs to %s, not %s", runtimeSnapshot.Repository, repository)
		}
		token, err := runtimeSnapshot.Token(ctx)
		if err != nil {
			return value, liveGitHubRuntimeSnapshot{}, "", fmt.Errorf("mint live GitHub writer token: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return value, liveGitHubRuntimeSnapshot{}, "", errors.New("mint live GitHub writer token: empty token")
		}
		return value, runtimeSnapshot, token, nil
	case string: // Narrow legacy test seam; dashboard handlers never pass tokens.
		token := strings.TrimSpace(value)
		if token == "" {
			return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, "", errors.New("test GitHub token is empty")
		}
		operator := hivegithub.AuthenticatedUserIdentity{ID: 1, Login: "test-owner", Type: "User"}
		return operator, liveGitHubRuntimeSnapshot{
			Client: dashboardLifecycleGitHubClient(token), Token: func(context.Context) (string, error) { return token, nil },
			Mode: "pat", Repository: repository, RepositoryID: 1, Writer: operator, BindingDigest: strings.Repeat("0", 64),
		}, token, nil
	default:
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, "", errors.New("dashboard lifecycle credential is invalid")
	}
}

// Dashboard mutations are durably bound before they begin. Once accepted,
// their completion must not depend on an ingress or browser keeping the HTTP
// connection open. The bounded child retains request values but survives a
// caller disconnect; exact request receipts make a later retry a replay.
func durableDashboardLifecycleContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), dashboardLifecycleMutationTimeout)
}

func dashboardIntegratedUninstalledRead(ctx context.Context, request dashboard.IntegratedLifecycleRequest, runtimeSnapshot liveGitHubRuntimeSnapshot) (map[string]any, error) {
	stateDir, err := dashboardSetupResetStateDir(request.Repository)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"schema_version": "hive.dashboard-integrated-" + request.Operation + ".v1",
		"repository":     request.Repository, "installed": false, "production_ready": false,
		"orphaned_setup": map[string]any{"detected": false, "recovery_available": false},
	}
	// Status and doctor are read-only. NewStore creates its directory, which
	// would turn a first uninstalled read into markerless repository state and
	// make the next read select the "-managed" recovery sibling. Only open a
	// recovery store after proving that it already exists.
	storeDir := filepath.Join(stateDir, "integrated")
	if _, statErr := os.Lstat(storeDir); statErr == nil {
		if store, storeErr := integrated.NewStore(storeDir); storeErr == nil {
			if intent, exists, intentErr := store.LoadUninstallIntent(); intentErr == nil && exists {
				result["orphaned_setup"] = map[string]any{
					"detected": true, "recovery_available": true, "finalization_available": true,
					"phase": intent.Phase, "cleanup_branch": intent.Branch, "cleanup_commit_sha": intent.CleanupCommitSHA,
					"pr_number": intent.PRNumber, "pr_url": intent.PRURL, "diff_digest": intent.DiffDigest,
				}
				augmentDashboardIntegratedRead(result, request.Repository, stateDir)
				return result, nil
			}
		}
	}
	plan, planErr := integrated.PlanOrphanedSetupReset(ctx, integrated.OrphanSetupResetOptions{
		Repository: request.Repository, StateDir: stateDir, GitHub: runtimeSnapshot.Client,
	})
	if planErr == nil {
		result["orphaned_setup"] = map[string]any{
			"detected": true, "recovery_available": true, "setup_pr_number": plan.SetupPRNumber,
			"setup_pr_url": plan.SetupPRURL, "current_head_sha": plan.CurrentHeadSHA, "plan_sha256": plan.PlanSHA256,
		}
	} else {
		digest := sha256.Sum256([]byte(planErr.Error()))
		result["orphaned_setup"] = map[string]any{
			"detected": false, "recovery_available": false, "inspection_reference": hex.EncodeToString(digest[:])[:12],
		}
	}
	augmentDashboardIntegratedRead(result, request.Repository, stateDir)
	return result, nil
}

func augmentDashboardIntegratedRead(result map[string]any, repository, stateDir string) {
	result["selected_state_root"] = filepath.Clean(integratedStateRoot())
	result["selected_state_dir"] = filepath.Clean(stateDir)
	validation := map[string]any{
		"hosted": config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "",
		"valid":  true,
	}
	if err := validateHostedIntegratedStateRoot(); err != nil {
		digest := sha256.Sum256([]byte(err.Error()))
		validation["valid"] = false
		validation["error_reference"] = hex.EncodeToString(digest[:])[:12]
	}
	result["persistence_validation"] = validation
	if receipt, exists, err := loadDashboardPreflightReceipt(repository); err == nil && exists {
		result["hosted_preflight"] = map[string]any{
			"ready": receipt.ExpiresAt.After(dashboardPreflightNow()), "binding_sha256": receipt.BindingSHA256,
			"state_root": receipt.StateRoot, "visual_hive_ref": receipt.VisualHiveRef,
			"operator_id": receipt.OperatorID, "operator_login": receipt.OperatorLogin,
			"writer_id": receipt.WriterID, "writer_login": receipt.WriterLogin, "writer_type": receipt.WriterType,
			"app_id": receipt.AppID, "installation_id": receipt.InstallationID, "permission_digest": receipt.PermissionDigest,
			"runtime_binding_digest": receipt.RuntimeBindingDigest,
			"tested_at":              receipt.TestedAt, "expires_at": receipt.ExpiresAt,
		}
	} else if err != nil {
		digest := sha256.Sum256([]byte(err.Error()))
		result["hosted_preflight"] = map[string]any{"ready": false, "error_reference": hex.EncodeToString(digest[:])[:12]}
	} else {
		result["hosted_preflight"] = map[string]any{"ready": false}
	}
	if _, exists := result["orphaned_setup"]; !exists {
		result["orphaned_setup"] = map[string]any{"detected": false, "recovery_available": false}
	}
	if runtimeSnapshot, ok := func() (liveGitHubRuntimeSnapshot, bool) {
		store := dashboardLiveGitHubRuntime.Load()
		if store == nil {
			return liveGitHubRuntimeSnapshot{}, false
		}
		return store.Current()
	}(); ok {
		result["github_runtime"] = map[string]any{
			"mode": runtimeSnapshot.Mode, "repository": runtimeSnapshot.Repository, "repository_id": runtimeSnapshot.RepositoryID,
			"writer_id": runtimeSnapshot.Writer.ID, "writer_login": runtimeSnapshot.Writer.Login, "writer_type": runtimeSnapshot.Writer.Type,
			"app_id": runtimeSnapshot.App.AppID, "installation_id": runtimeSnapshot.App.InstallationID,
			"permission_digest": runtimeSnapshot.App.PermissionDigest, "permissions": runtimeSnapshot.App.Permissions,
			"binding_digest": runtimeSnapshot.BindingDigest, "revision": runtimeSnapshot.Revision,
		}
	} else {
		result["github_runtime"] = map[string]any{"ready": false}
	}
	result["visual_runtime"] = dashboardNormalVisualRuntime.Load().RuntimeStatus()
}

func dashboardIntegratedStateDir(repository, action string) (string, error) {
	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil && action == "uninstall-finalize" {
		stateDir, stateErr := dashboardSetupStateDir(repository)
		if stateErr == nil {
			if recoverErr := integrated.RestoreInterruptedUninstallFinalization(stateDir, repository); recoverErr == nil {
				installed, exists, err = loadAuthoritativeVisualWorkContract()
			} else {
				err = recoverErr
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("load authoritative integrated lifecycle: %w", err)
	}
	if !exists {
		return "", errors.New("Visual Hive is not installed for this dashboard repository")
	}
	if !strings.EqualFold(strings.TrimSpace(installed.Repository), strings.TrimSpace(repository)) {
		return "", fmt.Errorf("installed integrated lifecycle belongs to %s, not the dashboard repository", installed.Repository)
	}
	if strings.TrimSpace(installed.StateDir) == "" {
		return "", errors.New("installed integrated lifecycle has no authoritative state directory")
	}
	return installed.StateDir, nil
}

func dashboardIntegratedControlPlan(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, operator hivegithub.AuthenticatedUserIdentity, runtimeSnapshot liveGitHubRuntimeSnapshot, token string) (map[string]any, error) {
	if request.Action == "" {
		return nil, errors.New("integrated control plan requires an exact action")
	}
	if request.Action == "setup-reset" || request.Action == "setup-reset-finalize" {
		return dashboardOrphanedSetupResetPlan(ctx, stateDir, request, operator, runtimeSnapshot)
	}
	status, _, err := dashboardLifecycleCLIRunner(ctx, []string{
		"status", "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
	}, token, false)
	if err != nil {
		return nil, fmt.Errorf("inspect current integrated lifecycle: %w", err)
	}
	statusBinding := dashboardStableControlStatus(status)
	statusBytes, err := json.Marshal(statusBinding)
	if err != nil {
		return nil, fmt.Errorf("bind current integrated lifecycle: %w", err)
	}
	statusSum := sha256.Sum256(statusBytes)
	plan := map[string]any{
		"schema_version":         "hive.dashboard-integrated-control-plan.v1",
		"repository":             request.Repository,
		"operation":              request.Action,
		"request_id":             request.RequestID,
		"current_status_sha256":  hex.EncodeToString(statusSum[:]),
		"current_status":         status,
		"operator_id":            operator.ID,
		"operator_login":         strings.ToLower(strings.TrimSpace(operator.Login)),
		"writer_id":              runtimeSnapshot.Writer.ID,
		"runtime_binding_digest": runtimeSnapshot.BindingDigest,
	}
	if request.Action == "repair-retire" {
		retirement, retirementErr := dashboardLifecyclePlanRetirement(ctx)
		if retirementErr != nil {
			return nil, retirementErr
		}
		plan["repair_retirement"] = retirement
	}
	planBinding := map[string]any{
		"schema_version":         plan["schema_version"],
		"repository":             request.Repository,
		"operation":              request.Action,
		"request_id":             request.RequestID,
		"current_status_sha256":  plan["current_status_sha256"],
		"operator_id":            operator.ID,
		"operator_login":         strings.ToLower(strings.TrimSpace(operator.Login)),
		"writer_id":              runtimeSnapshot.Writer.ID,
		"runtime_binding_digest": runtimeSnapshot.BindingDigest,
	}
	if retirement, exists := plan["repair_retirement"]; exists {
		planBinding["repair_retirement"] = retirement
	}
	planBytes, err := json.Marshal(planBinding)
	if err != nil {
		return nil, err
	}
	planSum := sha256.Sum256(planBytes)
	plan["plan_sha256"] = hex.EncodeToString(planSum[:])
	return plan, nil
}

// dashboardStableControlStatus binds only durable safety-relevant state.
// Human-readable health and age fields remain in the returned current_status
// for operator review but cannot make an otherwise unchanged plan drift
// between plan and apply.
func dashboardStableControlStatus(status map[string]any) map[string]any {
	binding := map[string]any{}
	for _, key := range []string{
		"config",
		"paused",
		"pause_requested",
		"runtime_owner",
		"runtime_owner_conflict",
	} {
		if value, exists := status[key]; exists {
			binding[key] = value
		}
	}
	lifecycle, _ := status["lifecycle_status"].(map[string]any)
	if lifecycle == nil {
		return binding
	}
	pending := map[string]any{}
	for _, key := range []string{
		"setup_authorizer_transfer",
		"setup_baseline_rebind",
		"setup_baseline",
		"uninstall",
		"hosted_release_transition",
	} {
		if value, exists := lifecycle[key]; exists {
			pending[key] = value
		}
	}
	if len(pending) != 0 {
		binding["pending_lifecycle"] = pending
	}
	return binding
}

func runDashboardIntegratedControl(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, operator hivegithub.AuthenticatedUserIdentity, runtimeSnapshot liveGitHubRuntimeSnapshot, token string, legacyTestCredential bool) (map[string]any, error) {
	ledgerStateDir := dashboardLifecycleLedgerStateDir(stateDir, request.Action)
	replay, terminal, recoverStale, err := replayDashboardLifecycleMutation(
		ledgerStateDir, request.RequestID, request.Action, request.ExpectedPlanSHA256,
	)
	if err != nil || terminal {
		return replay, err
	}
	planSHA256 := request.ExpectedPlanSHA256
	var authorizedRetirement *normalservice.RepairRetirementPlan
	if !recoverStale {
		plan, planErr := dashboardIntegratedControlPlan(ctx, stateDir, request, operator, runtimeSnapshot, token)
		if planErr != nil {
			return nil, planErr
		}
		planSHA256, _ = plan["plan_sha256"].(string)
		if request.ExpectedPlanSHA256 != planSHA256 {
			return nil, fmt.Errorf("integrated control plan changed: expected %s, current %s", request.ExpectedPlanSHA256, planSHA256)
		}
		if request.Action == "repair-retire" {
			value, ok := plan["repair_retirement"].(normalservice.RepairRetirementPlan)
			if !ok {
				return nil, errors.New("integrated repair retirement plan is missing its exact binding")
			}
			authorizedRetirement = &value
		}
	}
	return runDashboardLifecycleMutation(ledgerStateDir, request.RequestID, request.Action, planSHA256, func() (map[string]any, error) {
		switch request.Action {
		case "trigger":
			triggerErr := dashboardLifecycleTrigger(ctx)
			if triggerErr != nil && !isBoundedDashboardTriggerOutcome(triggerErr) {
				return nil, triggerErr
			}
			outcome := "completed"
			detail := ""
			if triggerErr != nil {
				outcome = "held"
				detail = triggerErr.Error()
			}
			return map[string]any{
				"schema_version": "hive.dashboard-integrated-trigger.v1",
				"repository":     request.Repository,
				"request_id":     request.RequestID,
				"outcome":        outcome,
				"detail":         detail,
			}, nil
		case "pause", "resume":
			result, _, runErr := dashboardLifecycleCLIRunner(ctx, []string{
				request.Action, "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
			}, token, false)
			return result, runErr
		case "repair-retire":
			if err := dashboardLifecycleRetireRepair(ctx, authorizedRetirement); err != nil {
				return nil, err
			}
			return map[string]any{
				"schema_version": "hive.dashboard-repair-retirement.v1",
				"repository":     request.Repository,
				"request_id":     request.RequestID,
				"outcome":        "retired_for_fresh_verification",
			}, nil
		case "uninstall", "uninstall-finalize", "uninstall-cancel":
			if (request.Action == "uninstall" || request.Action == "uninstall-finalize") && dashboardLifecycleStopNormalVisual != nil {
				if stopErr := dashboardLifecycleStopNormalVisual(ctx); stopErr != nil {
					return nil, fmt.Errorf("quiesce normal Visual Hive runtime before %s: %w", request.Action, stopErr)
				}
			}
			beadDirs := append([]string(nil), dashboardLifecycleNormalBeadsDirs...)
			if normalVisualRuntime := dashboardNormalVisualRuntime.Load(); normalVisualRuntime != nil {
				beadDirs = normalVisualRuntime.BeadDirs()
			}
			deleteState := request.Action == "uninstall-finalize"
			if legacyTestCredential {
				args := []string{"uninstall", "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN"}
				for _, directory := range beadDirs {
					args = append(args, "--beads-dir", directory)
				}
				if deleteState {
					args = append(args, "--delete-state")
				}
				if request.Action == "uninstall-cancel" {
					args = append(args, "--cancel")
				}
				result, _, runErr := dashboardLifecycleCLIRunner(ctx, args, token, false)
				return result, runErr
			}
			selectionCleared := false
			if deleteState {
				selectionCleared, err = integrated.ForgetCurrentState(integratedStateRoot(), stateDir)
				if err != nil {
					return nil, fmt.Errorf("retire current repository selection before uninstall finalization: %w", err)
				}
			}
			managementResult, runErr := integrated.RunManagement(ctx, integrated.ManagementOptions{
				Operation: integrated.OperationUninstall, StateDir: stateDir, LifecycleBeadsDirs: beadDirs,
				DeleteState: deleteState, Cancel: request.Action == "uninstall-cancel", GitHub: runtimeSnapshot.Client,
				GitTransportToken: token, AuthorizationActor: operator,
			})
			if runErr != nil && selectionCleared {
				if info, statErr := os.Stat(stateDir); statErr == nil && info.IsDir() {
					_ = integrated.RememberCurrentState(integratedStateRoot(), stateDir)
				}
			}
			result, encodeErr := dashboardLifecycleResultMap(managementResult)
			if encodeErr != nil {
				return nil, encodeErr
			}
			if normalVisualRuntime := dashboardNormalVisualRuntime.Load(); runErr != nil && normalVisualRuntime != nil {
				if _, resumeErr := normalVisualRuntime.ResumeReconciliation(ctx); resumeErr != nil {
					slog.Default().Warn("restore normal Visual Hive runtime after failed uninstall mutation", "error", resumeErr)
				}
			}
			if runErr == nil && request.Action == "uninstall-cancel" {
				result = reconcileDashboardSetupRuntime(ctx, result)
			}
			return result, runErr
		case "setup-reset":
			result, resetErr := integrated.PrepareOrphanedSetupReset(ctx, integrated.OrphanSetupResetOptions{
				Repository: request.Repository, StateDir: stateDir, GitHub: runtimeSnapshot.Client, GitTransportToken: token,
				AuthorizationActor: operator, RuntimeBindingDigest: runtimeSnapshot.BindingDigest,
			}, request.ExpectedPlanSHA256)
			if resetErr != nil {
				return nil, resetErr
			}
			encoded, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				return nil, encodeErr
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				return nil, err
			}
			response["schema_version"] = "hive.dashboard-orphaned-setup-reset.v1"
			response["request_id"] = request.RequestID
			return response, nil
		case "setup-reset-finalize":
			result, finalizeErr := integrated.RunManagement(ctx, integrated.ManagementOptions{
				Operation: integrated.OperationUninstall, StateDir: stateDir, DeleteState: true,
				GitHub: runtimeSnapshot.Client, GitTransportToken: token, AuthorizationActor: operator,
			})
			if finalizeErr != nil {
				return nil, finalizeErr
			}
			if !result.StateDeleted || !result.CleanupVerified {
				return nil, errors.New("orphaned setup reset finalization did not prove exact cleanup and state deletion")
			}
			if err := removeDashboardPreflightReceipt(request.Repository); err != nil {
				return nil, err
			}
			return map[string]any{
				"schema_version": "hive.dashboard-orphaned-setup-reset-finalize.v1", "repository": request.Repository,
				"request_id": request.RequestID, "cleanup_verified": true, "state_deleted": true,
			}, nil
		default:
			return nil, fmt.Errorf("unsupported integrated control action %q", request.Action)
		}
	})
}

func dashboardSetupResetStateDir(repository string) (string, error) {
	stateDir, err := dashboardSetupStateDir(repository)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "recovery", "orphaned-setup-reset"), nil
}

// Finalization deletes its operational state directory. Its request receipt
// must live in a persistent sibling so saving the completed receipt cannot
// silently recreate the state that was just proven deleted.
func dashboardLifecycleLedgerStateDir(stateDir, action string) string {
	switch action {
	case "uninstall-finalize", "setup-reset-finalize":
		return filepath.Clean(stateDir) + ".dashboard-finalization"
	default:
		return stateDir
	}
}

func dashboardOrphanedSetupResetPlan(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, operator hivegithub.AuthenticatedUserIdentity, runtimeSnapshot liveGitHubRuntimeSnapshot) (map[string]any, error) {
	if request.Action == "setup-reset-finalize" {
		store, storeErr := integrated.NewStore(filepath.Join(stateDir, "integrated"))
		if storeErr != nil {
			return nil, storeErr
		}
		intent, exists, intentErr := store.LoadUninstallIntent()
		if intentErr != nil || !exists {
			if intentErr == nil {
				intentErr = errors.New("no exact setup-reset cleanup is pending")
			}
			return nil, intentErr
		}
		binding := map[string]any{
			"schema_version": "hive.dashboard-orphaned-setup-reset-finalize-plan.v1", "repository": request.Repository,
			"operation": request.Action, "request_id": request.RequestID, "cleanup_branch": intent.Branch,
			"cleanup_commit_sha": intent.CleanupCommitSHA, "pr_number": intent.PRNumber, "diff_digest": intent.DiffDigest,
		}
		return bindDashboardOrphanPlan(binding, operator, runtimeSnapshot)
	}
	plan, err := integrated.PlanOrphanedSetupReset(ctx, integrated.OrphanSetupResetOptions{
		Repository: request.Repository, StateDir: stateDir, GitHub: runtimeSnapshot.Client,
		AuthorizationActor: operator, RuntimeBindingDigest: runtimeSnapshot.BindingDigest,
	})
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	response["operation"] = request.Action
	response["request_id"] = request.RequestID
	return response, nil
}

func bindDashboardOrphanPlan(plan map[string]any, operator hivegithub.AuthenticatedUserIdentity, runtimeSnapshot liveGitHubRuntimeSnapshot) (map[string]any, error) {
	delete(plan, "plan_sha256")
	plan["operator_id"] = operator.ID
	plan["operator_login"] = strings.ToLower(strings.TrimSpace(operator.Login))
	plan["writer_id"] = runtimeSnapshot.Writer.ID
	plan["runtime_binding_digest"] = runtimeSnapshot.BindingDigest
	data, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	plan["plan_sha256"] = hex.EncodeToString(digest[:])
	return plan, nil
}

func planDashboardRepairRetirement(ctx context.Context) (normalservice.RepairRetirementPlan, error) {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime == nil {
		return normalservice.RepairRetirementPlan{}, errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return normalVisualRuntime.PlanRepairRetirement(ctx)
}

func retireDashboardRepair(ctx context.Context, expected *normalservice.RepairRetirementPlan) error {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime == nil {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	return normalVisualRuntime.RetireRepair(ctx, expected)
}

func runDashboardBaselineApproval(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, operator hivegithub.AuthenticatedUserIdentity, runtimeSnapshot liveGitHubRuntimeSnapshot, token string, legacyTestCredential bool) (map[string]any, error) {
	approval := request.Baseline
	if approval == nil {
		return nil, errors.New("baseline approval bindings are missing")
	}
	return runDashboardLifecycleMutation(stateDir, approval.RequestID, "baseline-approve", approval.PlanDigest, func() (map[string]any, error) {
		if legacyTestCredential {
			args := []string{
				"approve-baseline", "--state-dir", stateDir, "--repo-id", approval.RepositoryID,
				"--run-id", fmt.Sprintf("%d", approval.CaptureRunID), "--artifact-id", fmt.Sprintf("%d", approval.ArtifactID),
				"--pr", fmt.Sprintf("%d", approval.PRNumber), "--head", approval.HeadSHA, "--base", approval.BaseSHA,
				"--diff-digest", approval.DiffDigest, "--candidate-digest", approval.CandidateDigest,
				"--actor-id", fmt.Sprintf("%d", approval.ActorID), "--plan-digest", approval.PlanDigest,
				"--reason", approval.Reason, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
			}
			result, _, runErr := dashboardLifecycleCLIRunner(ctx, args, token, false)
			return result, runErr
		}
		result, runErr := integrated.ApproveSetupBaseline(ctx, integrated.ApproveSetupBaselineOptions{
			StateDir: stateDir, ExpectedRepositoryID: approval.RepositoryID, ExpectedCaptureRunID: approval.CaptureRunID,
			ExpectedArtifactID: approval.ArtifactID, ExpectedPRNumber: approval.PRNumber, ExpectedHeadSHA: approval.HeadSHA,
			ExpectedBaseSHA: approval.BaseSHA, ExpectedDiffDigest: approval.DiffDigest, ExpectedCandidateDigest: approval.CandidateDigest,
			ExpectedActorID: approval.ActorID, ExpectedPlanDigest: approval.PlanDigest, Reason: approval.Reason,
			GitHub: runtimeSnapshot.Client, GitTransportToken: token, AuthorizationActor: operator,
		})
		if runErr != nil {
			return nil, runErr
		}
		return dashboardLifecycleResultMap(result)
	})
}

func dashboardLifecycleResultMap(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func triggerDashboardVisualCycle(ctx context.Context) error {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime == nil {
		return errors.New("normal Visual Hive service is not running in this dashboard process")
	}
	triggerCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	return normalVisualRuntime.Trigger(triggerCtx)
}

func isBoundedDashboardTriggerOutcome(err error) bool {
	return errors.Is(err, normalservice.ErrNoDispatch) ||
		errors.Is(err, normalservice.ErrOpenPullRequest) ||
		errors.Is(err, normalservice.ErrRepairRetirement) ||
		errors.Is(err, normalservice.ErrFinalVerdictPending) ||
		errors.Is(err, integrated.ErrRunInProgress) ||
		errors.Is(err, integrated.ErrNormalVisualSetupBaselinePending) ||
		errors.Is(err, integrated.ErrSetupBaselineLifecycleHold)
}

func runDashboardLifecycleCLI(parent context.Context, args []string, token string, allowNonzeroJSON bool) (map[string]any, []byte, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, errors.New("owner GitHub authorization is empty")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 50*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = dashboardSetupEnvironment(token)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	var result map[string]any
	decodeErr := json.Unmarshal(stdout.Bytes(), &result)
	if runErr == nil || allowNonzeroJSON && decodeErr == nil {
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("integrated lifecycle subprocess returned invalid JSON: %w", decodeErr)
		}
		return result, append([]byte(nil), stdout.Bytes()...), nil
	}
	diagnostic := boundedMCPDiagnostic(stderr.Bytes())
	if diagnostic == "" && decodeErr == nil {
		if value, ok := result["error"].(string); ok {
			diagnostic = value
		}
	}
	if diagnostic == "" {
		diagnostic = "integrated lifecycle subprocess failed without a diagnostic"
	}
	return nil, nil, errors.New(diagnostic)
}

func runDashboardLifecycleMutation(stateDir, requestID, operation, planSHA256 string, run func() (map[string]any, error)) (map[string]any, error) {
	dashboardLifecycleMu.Lock()
	defer dashboardLifecycleMu.Unlock()
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil {
		return nil, err
	}
	recordIndex := -1
	for index, entry := range ledger.Entries {
		if entry.RequestID != requestID {
			continue
		}
		result, proceed, entryErr := inspectDashboardLifecycleRecord(entry, operation, planSHA256)
		if entryErr != nil || !proceed {
			return result, entryErr
		}
		recordIndex = index
	}
	record := dashboardLifecycleRequestRecord{
		RequestID: requestID, Operation: operation, PlanSHA256: planSHA256,
		Status: "started", StartedAt: time.Now().UTC(),
	}
	if recordIndex >= 0 {
		ledger.Entries[recordIndex] = record
	} else {
		ledger.Entries = append(ledger.Entries, record)
	}
	if err := saveDashboardLifecycleLedger(stateDir, ledger); err != nil {
		return nil, err
	}
	result, runErr := run()
	for index := range ledger.Entries {
		entry := &ledger.Entries[index]
		if entry.RequestID != requestID {
			continue
		}
		entry.CompletedAt = time.Now().UTC()
		if runErr != nil {
			sum := sha256.Sum256([]byte(runErr.Error()))
			entry.Status = "failed"
			entry.ErrorSHA256 = hex.EncodeToString(sum[:])
		} else {
			bytes, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				runErr = marshalErr
				sum := sha256.Sum256([]byte(runErr.Error()))
				entry.Status = "failed"
				entry.ErrorSHA256 = hex.EncodeToString(sum[:])
			} else {
				entry.Status = "completed"
				entry.Result = bytes
			}
		}
		break
	}
	if saveErr := saveDashboardLifecycleLedger(stateDir, ledger); saveErr != nil {
		return nil, saveErr
	}
	return result, runErr
}

// replayDashboardLifecycleMutation reports terminal=true for a completed,
// failed, or still-running exact request. recoverStale=true means the exact
// request has an old started receipt and must retry only its already-bound
// idempotent operation; callers must not re-plan against potentially changed
// post-operation state.
func replayDashboardLifecycleMutation(stateDir, requestID, operation, planSHA256 string) (map[string]any, bool, bool, error) {
	dashboardLifecycleMu.Lock()
	defer dashboardLifecycleMu.Unlock()
	ledger, err := loadDashboardLifecycleLedger(stateDir)
	if err != nil {
		return nil, false, false, err
	}
	for _, entry := range ledger.Entries {
		if entry.RequestID != requestID {
			continue
		}
		result, proceed, entryErr := inspectDashboardLifecycleRecord(entry, operation, planSHA256)
		return result, !proceed, proceed, entryErr
	}
	return nil, false, false, nil
}

// inspectDashboardLifecycleRecord returns proceed=true only for an exact stale
// started receipt that is safe to recover through the underlying idempotent
// Hive operation.
func inspectDashboardLifecycleRecord(entry dashboardLifecycleRequestRecord, operation, planSHA256 string) (map[string]any, bool, error) {
	if entry.Operation != operation || entry.PlanSHA256 != planSHA256 {
		return nil, false, errors.New("dashboard request_id is already bound to a different operation or exact plan")
	}
	switch entry.Status {
	case "completed":
		var result map[string]any
		if err := json.Unmarshal(entry.Result, &result); err != nil || result == nil {
			return nil, false, errors.New("completed dashboard lifecycle receipt is invalid")
		}
		result["idempotent_replay"] = true
		return result, false, nil
	case "started":
		if time.Since(entry.StartedAt) < 55*time.Minute {
			return nil, false, errors.New("the exact dashboard lifecycle request is already in progress")
		}
		return nil, true, nil
	case "failed":
		reference := entry.ErrorSHA256
		if len(reference) > 12 {
			reference = reference[:12]
		}
		return nil, false, fmt.Errorf("the exact dashboard lifecycle request already failed (reference %s); resolve the cause and use a new request_id", reference)
	default:
		return nil, false, errors.New("dashboard lifecycle request ledger contains an invalid status")
	}
}

func loadDashboardLifecycleLedger(stateDir string) (dashboardLifecycleLedger, error) {
	path, err := dashboardLifecycleLedgerPath(stateDir)
	if err != nil {
		return dashboardLifecycleLedger{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return dashboardLifecycleLedger{SchemaVersion: dashboardLifecycleLedgerSchema}, nil
	}
	if err != nil {
		return dashboardLifecycleLedger{}, fmt.Errorf("read dashboard lifecycle request ledger: %w", err)
	}
	var ledger dashboardLifecycleLedger
	if err := json.Unmarshal(data, &ledger); err != nil || ledger.SchemaVersion != dashboardLifecycleLedgerSchema {
		return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger is invalid")
	}
	if len(ledger.Entries) > 10000 {
		return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger exceeds its bounded entry count")
	}
	for _, entry := range ledger.Entries {
		if strings.TrimSpace(entry.RequestID) == "" || strings.TrimSpace(entry.Operation) == "" || len(entry.PlanSHA256) != 64 || entry.StartedAt.IsZero() {
			return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger contains an invalid binding")
		}
		switch entry.Status {
		case "started":
		case "completed":
			var result map[string]any
			if len(entry.Result) == 0 || json.Unmarshal(entry.Result, &result) != nil || result == nil || entry.CompletedAt.IsZero() {
				return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger contains an invalid completed receipt")
			}
		case "failed":
			if len(entry.ErrorSHA256) != 64 || entry.CompletedAt.IsZero() {
				return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger contains an invalid failure receipt")
			}
		default:
			return dashboardLifecycleLedger{}, errors.New("dashboard lifecycle request ledger contains an invalid status")
		}
	}
	return ledger, nil
}

func saveDashboardLifecycleLedger(stateDir string, ledger dashboardLifecycleLedger) error {
	ledger.SchemaVersion = dashboardLifecycleLedgerSchema
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path, err := dashboardLifecycleLedgerPath(stateDir)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".dashboard-control-requests-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return nil
}

// dashboardLifecycleLedgerPath keeps dashboard transaction receipts beside,
// rather than inside, the uninstallable repository state. The state-path
// digest isolates repositories on the same persistent volume. Final uninstall
// can therefore remove the exact installed state without recreating it while
// a lost successful response remains safely replayable.
func dashboardLifecycleLedgerPath(stateDir string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil || strings.TrimSpace(stateDir) == "" {
		return "", errors.New("dashboard lifecycle state directory is invalid")
	}
	absolute = filepath.Clean(absolute)
	binding := absolute
	if runtime.GOOS == "windows" {
		binding = strings.ToLower(binding)
	}
	sum := sha256.Sum256([]byte(binding))
	return filepath.Join(
		filepath.Dir(absolute),
		".hive-dashboard-control",
		hex.EncodeToString(sum[:])+".json",
	), nil
}

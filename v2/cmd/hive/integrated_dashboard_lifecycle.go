package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/dashboard"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

const dashboardLifecycleLedgerSchema = "hive.dashboard-integrated-requests.v1"

var (
	dashboardLifecycleCLIRunner        = runDashboardLifecycleCLI
	dashboardLifecycleTrigger          = triggerDashboardVisualCycle
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

func runDashboardIntegratedLifecycle(ctx context.Context, request dashboard.IntegratedLifecycleRequest, token string) (map[string]any, error) {
	stateDir := ""
	if request.Operation == "control-apply" {
		candidateStateDir, candidateErr := dashboardSetupStateDir(request.Repository)
		if candidateErr == nil {
			replay, terminal, recoverStale, replayErr := replayDashboardLifecycleMutation(
				candidateStateDir, request.RequestID, request.Action, request.ExpectedPlanSHA256,
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
		return result, runErr
	case "baseline-plan":
		result, _, runErr := dashboardLifecycleCLIRunner(ctx, []string{
			"approve-baseline", "--state-dir", stateDir, "--plan", "--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
		}, token, false)
		return result, runErr
	case "baseline-approve":
		return runDashboardBaselineApproval(ctx, stateDir, request, token)
	case "control-plan":
		return dashboardIntegratedControlPlan(ctx, stateDir, request, token)
	case "control-apply":
		return runDashboardIntegratedControl(ctx, stateDir, request, token)
	default:
		return nil, fmt.Errorf("unsupported integrated dashboard operation %q", request.Operation)
	}
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

func dashboardIntegratedControlPlan(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, token string) (map[string]any, error) {
	if request.Action == "" {
		return nil, errors.New("integrated control plan requires an exact action")
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
		"schema_version":        "hive.dashboard-integrated-control-plan.v1",
		"repository":            request.Repository,
		"operation":             request.Action,
		"request_id":            request.RequestID,
		"current_status_sha256": hex.EncodeToString(statusSum[:]),
		"current_status":        status,
	}
	planBinding := map[string]any{
		"schema_version":        plan["schema_version"],
		"repository":            request.Repository,
		"operation":             request.Action,
		"request_id":            request.RequestID,
		"current_status_sha256": plan["current_status_sha256"],
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

func runDashboardIntegratedControl(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, token string) (map[string]any, error) {
	replay, terminal, recoverStale, err := replayDashboardLifecycleMutation(
		stateDir, request.RequestID, request.Action, request.ExpectedPlanSHA256,
	)
	if err != nil || terminal {
		return replay, err
	}
	planSHA256 := request.ExpectedPlanSHA256
	if !recoverStale {
		plan, planErr := dashboardIntegratedControlPlan(ctx, stateDir, request, token)
		if planErr != nil {
			return nil, planErr
		}
		planSHA256, _ = plan["plan_sha256"].(string)
		if request.ExpectedPlanSHA256 != planSHA256 {
			return nil, fmt.Errorf("integrated control plan changed: expected %s, current %s", request.ExpectedPlanSHA256, planSHA256)
		}
	}
	return runDashboardLifecycleMutation(stateDir, request.RequestID, request.Action, planSHA256, func() (map[string]any, error) {
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
		case "uninstall", "uninstall-finalize", "uninstall-cancel":
			if (request.Action == "uninstall" || request.Action == "uninstall-finalize") && dashboardLifecycleStopNormalVisual != nil {
				if stopErr := dashboardLifecycleStopNormalVisual(ctx); stopErr != nil {
					return nil, fmt.Errorf("quiesce normal Visual Hive runtime before %s: %w", request.Action, stopErr)
				}
			}
			args := []string{"uninstall", "--state-dir", stateDir, "--json", "--github-token-env", "HIVE_GITHUB_TOKEN"}
			if request.Action != "uninstall-cancel" {
				beadDirs := append([]string(nil), dashboardLifecycleNormalBeadsDirs...)
				if normalVisualRuntime := dashboardNormalVisualRuntime.Load(); normalVisualRuntime != nil {
					beadDirs = normalVisualRuntime.BeadDirs()
				}
				for _, dir := range beadDirs {
					if strings.TrimSpace(dir) != "" {
						args = append(args, "--beads-dir", dir)
					}
				}
			}
			switch request.Action {
			case "uninstall-finalize":
				args = append(args, "--delete-state")
			case "uninstall-cancel":
				args = append(args, "--cancel")
			}
			result, _, runErr := dashboardLifecycleCLIRunner(ctx, args, token, false)
			if normalVisualRuntime := dashboardNormalVisualRuntime.Load(); runErr != nil && normalVisualRuntime != nil {
				if _, resumeErr := normalVisualRuntime.ResumeReconciliation(ctx); resumeErr != nil {
					slog.Default().Warn("restore normal Visual Hive runtime after failed uninstall mutation", "error", resumeErr)
				}
			}
			if runErr == nil && request.Action == "uninstall-cancel" {
				result = reconcileDashboardSetupRuntime(ctx, result)
			}
			return result, runErr
		default:
			return nil, fmt.Errorf("unsupported integrated control action %q", request.Action)
		}
	})
}

func runDashboardBaselineApproval(ctx context.Context, stateDir string, request dashboard.IntegratedLifecycleRequest, token string) (map[string]any, error) {
	approval := request.Baseline
	if approval == nil {
		return nil, errors.New("baseline approval bindings are missing")
	}
	args := []string{
		"approve-baseline", "--state-dir", stateDir,
		"--repo-id", approval.RepositoryID,
		"--run-id", fmt.Sprint(approval.CaptureRunID),
		"--artifact-id", fmt.Sprint(approval.ArtifactID),
		"--pr", fmt.Sprint(approval.PRNumber),
		"--head", approval.HeadSHA,
		"--base", approval.BaseSHA,
		"--diff-digest", approval.DiffDigest,
		"--candidate-digest", approval.CandidateDigest,
		"--actor-id", fmt.Sprint(approval.ActorID),
		"--plan-digest", approval.PlanDigest,
		"--reason", approval.Reason,
		"--json", "--github-token-env", "HIVE_GITHUB_TOKEN",
	}
	return runDashboardLifecycleMutation(stateDir, approval.RequestID, "baseline-approve", approval.PlanDigest, func() (map[string]any, error) {
		result, _, runErr := dashboardLifecycleCLIRunner(ctx, args, token, false)
		return result, runErr
	})
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

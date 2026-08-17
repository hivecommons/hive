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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/dashboard"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

var dashboardSetupCLIRunner = runDashboardSetupCLI

func runDashboardIntegratedSetup(ctx context.Context, request dashboard.IntegratedSetupRequest, credential any) (map[string]any, error) {
	operator, runtime, err := resolveDashboardSetupCredential(ctx, request.Repository, credential)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(runtime.Repository, request.Repository) {
		return nil, errors.New("live GitHub runtime is bound to a different repository")
	}
	if runtime.Mode == "app" {
		if err := runtime.App.RequireVisualHivePermissions(); err != nil {
			return nil, fmt.Errorf("live GitHub App permissions do not permit integrated setup: %w", err)
		}
	}
	token, err := runtime.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("mint live GitHub writer token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("mint live GitHub writer token: empty token")
	}
	if request.ExpectedPlanSHA256 != "" {
		if err := requireDashboardPreflightReceipt(ctx, request, operator, runtime); err != nil {
			return nil, err
		}
		mutationCtx, cancel := durableDashboardLifecycleContext(ctx)
		defer cancel()
		ctx = mutationCtx
	}
	baseArgs := []string{
		"setup", "--json",
		"--repo", request.Repository,
		"--coverage", request.Coverage,
		"--automation", request.Automation,
		"--provider", request.Provider,
		"--runtime", "local",
		"--visual-hive=true",
		"--visual-hive-ref", request.VisualHiveRef,
		"--max-active-issues", strconv.Itoa(dashboardSetupMaxActiveIssues(request)),
	}
	stateDir, err := dashboardSetupStateDir(request.Repository)
	if err != nil {
		return nil, err
	}
	if request.ExpectedPlanSHA256 != "" {
		replay, terminal, recoverStale, replayErr := replayDashboardLifecycleMutation(
			stateDir, request.RequestID, "setup", request.ExpectedPlanSHA256,
		)
		if replayErr != nil || terminal {
			if replayErr == nil {
				replay = reconcileDashboardSetupRuntime(ctx, replay)
			}
			return replay, replayErr
		}
		if recoverStale {
			return runDashboardSetupMutationWithRuntimeRebind(ctx, stateDir, request, baseArgs, token, operator, runtime)
		}
	}
	planArgs := append(append([]string(nil), baseArgs...), "--plan")
	if runtime.Mode == "app" {
		intentDigest, digestErr := dashboardSetupIntentDigest(request, operator, runtime)
		if digestErr != nil {
			return nil, fmt.Errorf("bind dashboard setup plan request: %w", digestErr)
		}
		handoffPath, handoffErr := createDashboardOperatorHandoff(ctx, request.Repository, request.RequestID, intentDigest, operator, runtime)
		if handoffErr != nil {
			return nil, fmt.Errorf("seal dashboard setup plan operator identity: %w", handoffErr)
		}
		defer func() {
			_ = dashboardOperatorHandoffExec(context.Background(), removeOperatorHandoffCommand, handoffPath, nil)
		}()
		planArgs = append(planArgs,
			"--dashboard-operator-handoff", handoffPath,
			"--dashboard-request-id", request.RequestID,
			"--dashboard-plan-sha256", intentDigest,
		)
	}
	plan, planBytes, err := dashboardSetupCLIRunner(ctx, planArgs, token)
	if err != nil {
		return nil, fmt.Errorf("integrated setup plan failed: %w", err)
	}
	planSHA256, err := dashboardSetupPlanDigest(request, operator, runtime, planBytes)
	if err != nil {
		return nil, fmt.Errorf("canonicalize integrated setup plan: %w", err)
	}
	if request.ExpectedPlanSHA256 == "" {
		return map[string]any{
			"schema_version": "hive.dashboard-integrated-setup-plan.v1",
			"request_id":     request.RequestID,
			"plan_sha256":    planSHA256,
			"result":         plan,
		}, nil
	}
	if request.ExpectedPlanSHA256 != planSHA256 {
		return nil, fmt.Errorf("integrated setup plan changed: expected %s, current %s", request.ExpectedPlanSHA256, planSHA256)
	}
	return runDashboardSetupMutationWithRuntimeRebind(ctx, stateDir, request, baseArgs, token, operator, runtime)
}

// resolveDashboardSetupCredential keeps the production boundary strongly
// identity-based while preserving the narrow token seam used by the existing
// package tests. Dashboard handlers always pass an AuthenticatedUserIdentity.
func resolveDashboardSetupCredential(ctx context.Context, repository string, credential any) (hivegithub.AuthenticatedUserIdentity, liveGitHubRuntimeSnapshot, error) {
	switch value := credential.(type) {
	case hivegithub.AuthenticatedUserIdentity:
		runtime, err := currentDashboardGitHubRuntime()
		if err != nil {
			return value, liveGitHubRuntimeSnapshot{}, err
		}
		if runtime.Mode == "app" {
			runtime, err = refreshLiveGitHubAppRuntime(ctx, runtime)
		}
		return value, runtime, err
	case string:
		token := strings.TrimSpace(value)
		if token == "" {
			return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, errors.New("dashboard setup test credential is empty")
		}
		operator := hivegithub.AuthenticatedUserIdentity{ID: 1, Login: "test-owner", Type: "User"}
		return operator, liveGitHubRuntimeSnapshot{
			Token: func(context.Context) (string, error) { return token, nil }, Mode: "pat",
			Repository: repository, RepositoryID: 1, Writer: operator, BindingDigest: strings.Repeat("0", 64),
		}, nil
	default:
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, errors.New("dashboard setup credential is invalid")
	}
}

func dashboardSetupIntentDigest(request dashboard.IntegratedSetupRequest, operator hivegithub.AuthenticatedUserIdentity, runtime liveGitHubRuntimeSnapshot) (string, error) {
	payload := struct {
		Schema          string `json:"schema"`
		RequestID       string `json:"request_id"`
		Repository      string `json:"repository"`
		Coverage        string `json:"coverage"`
		Automation      string `json:"automation"`
		Provider        string `json:"provider"`
		VisualHiveRef   string `json:"visual_hive_ref"`
		MaxActiveIssues int    `json:"max_active_issues"`
		OperatorID      int64  `json:"operator_id"`
		OperatorLogin   string `json:"operator_login"`
		RuntimeBinding  string `json:"runtime_binding"`
	}{
		Schema: "hive.dashboard-integrated-setup-intent.v1", RequestID: request.RequestID,
		Repository: strings.ToLower(strings.TrimSpace(request.Repository)), Coverage: request.Coverage,
		Automation: request.Automation, Provider: request.Provider, VisualHiveRef: strings.ToLower(strings.TrimSpace(request.VisualHiveRef)),
		MaxActiveIssues: dashboardSetupMaxActiveIssues(request), OperatorID: operator.ID,
		OperatorLogin: strings.ToLower(strings.TrimSpace(operator.Login)), RuntimeBinding: runtime.BindingDigest,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func runDashboardSetupMutationWithRuntimeRebind(
	ctx context.Context,
	stateDir string,
	request dashboard.IntegratedSetupRequest,
	baseArgs []string,
	token string,
	operator hivegithub.AuthenticatedUserIdentity,
	runtime liveGitHubRuntimeSnapshot,
) (map[string]any, error) {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime != nil {
		if err := normalVisualRuntime.Stop(ctx); err != nil {
			return nil, fmt.Errorf("quiesce normal Visual Hive runtime before managed setup apply: %w", err)
		}
	}
	applyArgs := append([]string(nil), baseArgs...)
	if runtime.Mode == "app" {
		handoffPath, handoffErr := createDashboardOperatorHandoff(ctx, request.Repository, request.RequestID, request.ExpectedPlanSHA256, operator, runtime)
		if handoffErr != nil {
			return nil, fmt.Errorf("seal dashboard setup operator identity: %w", handoffErr)
		}
		defer func() {
			_ = dashboardOperatorHandoffExec(context.Background(), removeOperatorHandoffCommand, handoffPath, nil)
		}()
		applyArgs = append(applyArgs,
			"--dashboard-operator-handoff", handoffPath,
			"--dashboard-request-id", request.RequestID,
			"--dashboard-plan-sha256", request.ExpectedPlanSHA256,
		)
	}
	result, mutationErr := runDashboardSetupMutation(ctx, stateDir, request, applyArgs, token)
	if mutationErr != nil {
		if normalVisualRuntime != nil {
			if _, resumeErr := normalVisualRuntime.ResumeReconciliation(ctx); resumeErr != nil {
				return result, errors.Join(mutationErr, fmt.Errorf("restore normal Visual Hive runtime after failed managed setup apply: %w", resumeErr))
			}
		}
		return result, mutationErr
	}
	return reconcileDashboardSetupRuntime(ctx, result), nil
}

func reconcileDashboardSetupRuntime(ctx context.Context, result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	activation := map[string]any{"state": "pending"}
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime == nil {
		activation["error_reference"] = dashboardSetupErrorDigest(errors.New("normal Visual Hive runtime manager is unavailable"))
		result["visual_runtime_activation"] = activation
		return result
	}
	active, err := normalVisualRuntime.ResumeReconciliation(ctx)
	switch {
	case err != nil:
		activation["error_reference"] = dashboardSetupErrorDigest(err)
	case active:
		activation["state"] = "ready"
	default:
		activation["error_reference"] = dashboardSetupErrorDigest(errors.New("authoritative Visual Hive contract is not yet available"))
	}
	result["visual_runtime_activation"] = activation
	return result
}

func dashboardSetupErrorDigest(err error) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(err)))
	return hex.EncodeToString(sum[:])
}

func dashboardSetupPlanDigest(request dashboard.IntegratedSetupRequest, arguments ...any) (string, error) {
	operator, runtime, planBytes, err := dashboardSetupPlanDigestArguments(request.Repository, arguments)
	if err != nil {
		return "", err
	}
	var planEnvelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(planBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&planEnvelope); err != nil {
		return "", fmt.Errorf("decode setup plan: %w", err)
	}
	if err := ensureDashboardSetupJSONEOF(decoder); err != nil {
		return "", err
	}
	if plan, ok := planEnvelope["plan"].(map[string]any); ok {
		// SetupPlan.GeneratedAt describes when the read-only inspection ran; it
		// is not a plan decision. Binding the dashboard digest to that timestamp
		// made every plan impossible to apply because apply intentionally
		// recomputes a fresh plan before mutating the repository.
		delete(plan, "generated_at")
	}
	canonicalPlan, err := json.Marshal(planEnvelope)
	if err != nil {
		return "", fmt.Errorf("encode canonical setup plan: %w", err)
	}
	binding := strings.Join([]string{
		"hive.dashboard-integrated-setup-plan.v5",
		request.RequestID,
		request.Repository,
		request.Coverage,
		request.Automation,
		request.Provider,
		request.VisualHiveRef,
		strconv.Itoa(dashboardSetupMaxActiveIssues(request)),
		strconv.FormatInt(operator.ID, 10),
		strings.ToLower(strings.TrimSpace(operator.Login)),
		strconv.FormatInt(runtime.Writer.ID, 10),
		strings.ToLower(strings.TrimSpace(runtime.Writer.Login)),
		strings.ToLower(strings.TrimSpace(runtime.Writer.Type)),
		runtime.BindingDigest,
	}, "\n") + "\n"
	sum := sha256.Sum256(append([]byte(binding), canonicalPlan...))
	return hex.EncodeToString(sum[:]), nil
}

func dashboardSetupPlanDigestArguments(repository string, arguments []any) (hivegithub.AuthenticatedUserIdentity, liveGitHubRuntimeSnapshot, []byte, error) {
	if len(arguments) == 1 {
		planBytes, ok := arguments[0].([]byte)
		if !ok {
			return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, nil, errors.New("setup plan digest input is invalid")
		}
		operator := hivegithub.AuthenticatedUserIdentity{ID: 1, Login: "test-owner", Type: "User"}
		return operator, liveGitHubRuntimeSnapshot{
			Mode: "pat", Repository: repository, RepositoryID: 1, Writer: operator, BindingDigest: strings.Repeat("0", 64),
		}, planBytes, nil
	}
	if len(arguments) != 3 {
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, nil, errors.New("setup plan digest binding is incomplete")
	}
	operator, operatorOK := arguments[0].(hivegithub.AuthenticatedUserIdentity)
	runtime, runtimeOK := arguments[1].(liveGitHubRuntimeSnapshot)
	planBytes, planOK := arguments[2].([]byte)
	if !operatorOK || !runtimeOK || !planOK {
		return hivegithub.AuthenticatedUserIdentity{}, liveGitHubRuntimeSnapshot{}, nil, errors.New("setup plan digest binding is invalid")
	}
	return operator, runtime, planBytes, nil
}

func dashboardSetupMaxActiveIssues(request dashboard.IntegratedSetupRequest) int {
	if request.MaxActiveIssues == nil {
		return 5
	}
	return *request.MaxActiveIssues
}

func ensureDashboardSetupJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("setup plan contains more than one JSON value")
	}
	return fmt.Errorf("decode setup plan trailer: %w", err)
}

func dashboardSetupStateDir(repository string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve dashboard setup state directory: %w", err)
		}
		return filepath.Clean(absolute), nil
	}
	return repositoryIntegratedStateDir(repository)
}

func runDashboardSetupMutation(
	ctx context.Context,
	stateDir string,
	request dashboard.IntegratedSetupRequest,
	baseArgs []string,
	token string,
) (map[string]any, error) {
	return runDashboardLifecycleMutation(stateDir, request.RequestID, "setup", request.ExpectedPlanSHA256, func() (map[string]any, error) {
		applied, _, err := dashboardSetupCLIRunner(ctx, baseArgs, token)
		if err != nil {
			return nil, fmt.Errorf("integrated setup apply failed: %w", err)
		}
		return map[string]any{
			"schema_version": "hive.dashboard-integrated-setup-apply.v1",
			"request_id":     request.RequestID,
			"plan_sha256":    request.ExpectedPlanSHA256,
			"result":         applied,
		}, nil
	})
}

func runDashboardSetupCLI(parent context.Context, args []string, token string) (map[string]any, []byte, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, errors.New("owner GitHub authorization is empty")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = dashboardSetupEnvironment(token)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if runErr != nil {
		diagnostic := boundedMCPDiagnostic(stderr.Bytes())
		if diagnostic == "" {
			diagnostic = "setup subprocess failed without a diagnostic"
		}
		return nil, nil, errors.New(diagnostic)
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, nil, fmt.Errorf("setup subprocess returned invalid JSON: %w", err)
	}
	return result, append([]byte(nil), stdout.Bytes()...), nil
}

func dashboardSetupEnvironment(token string) []string {
	const tokenName = "HIVE_GITHUB_TOKEN"
	result := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		name, _, found := strings.Cut(value, "=")
		if found && strings.EqualFold(name, tokenName) {
			continue
		}
		result = append(result, value)
	}
	return append(result, tokenName+"="+token)
}

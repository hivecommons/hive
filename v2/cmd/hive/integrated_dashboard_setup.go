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
)

var dashboardSetupCLIRunner = runDashboardSetupCLI

func runDashboardIntegratedSetup(ctx context.Context, request dashboard.IntegratedSetupRequest, token string) (map[string]any, error) {
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
			return runDashboardSetupMutationWithRuntimeRebind(ctx, stateDir, request, baseArgs, token)
		}
	}
	plan, planBytes, err := dashboardSetupCLIRunner(ctx, append(append([]string(nil), baseArgs...), "--plan"), token)
	if err != nil {
		return nil, fmt.Errorf("integrated setup plan failed: %w", err)
	}
	planSHA256, err := dashboardSetupPlanDigest(request, planBytes)
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
	return runDashboardSetupMutationWithRuntimeRebind(ctx, stateDir, request, baseArgs, token)
}

func runDashboardSetupMutationWithRuntimeRebind(
	ctx context.Context,
	stateDir string,
	request dashboard.IntegratedSetupRequest,
	baseArgs []string,
	token string,
) (map[string]any, error) {
	normalVisualRuntime := dashboardNormalVisualRuntime.Load()
	if normalVisualRuntime != nil {
		if err := normalVisualRuntime.Stop(ctx); err != nil {
			return nil, fmt.Errorf("quiesce normal Visual Hive runtime before managed setup apply: %w", err)
		}
	}
	result, mutationErr := runDashboardSetupMutation(ctx, stateDir, request, baseArgs, token)
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

func dashboardSetupPlanDigest(request dashboard.IntegratedSetupRequest, planBytes []byte) (string, error) {
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
		"hive.dashboard-integrated-setup-plan.v4",
		request.RequestID,
		request.Repository,
		request.Coverage,
		request.Automation,
		request.Provider,
		request.VisualHiveRef,
		strconv.Itoa(dashboardSetupMaxActiveIssues(request)),
	}, "\n") + "\n"
	sum := sha256.Sum256(append([]byte(binding), canonicalPlan...))
	return hex.EncodeToString(sum[:]), nil
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

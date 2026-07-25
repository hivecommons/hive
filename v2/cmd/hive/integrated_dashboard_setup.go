package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
			return replay, replayErr
		}
		if recoverStale {
			return runDashboardSetupMutation(ctx, stateDir, request, baseArgs, token)
		}
	}
	plan, planBytes, err := dashboardSetupCLIRunner(ctx, append(append([]string(nil), baseArgs...), "--plan"), token)
	if err != nil {
		return nil, fmt.Errorf("integrated setup plan failed: %w", err)
	}
	planSHA256 := dashboardSetupPlanDigest(request, planBytes)
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
	return runDashboardSetupMutation(ctx, stateDir, request, baseArgs, token)
}

func dashboardSetupPlanDigest(request dashboard.IntegratedSetupRequest, planBytes []byte) string {
	binding := strings.Join([]string{
		"hive.dashboard-integrated-setup-plan.v2",
		request.RequestID,
		request.Repository,
		request.Coverage,
		request.Automation,
		request.Provider,
		request.VisualHiveRef,
	}, "\n") + "\n"
	sum := sha256.Sum256(append([]byte(binding), planBytes...))
	return hex.EncodeToString(sum[:])
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

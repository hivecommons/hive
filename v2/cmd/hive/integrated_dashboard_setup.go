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
	plan, planBytes, err := dashboardSetupCLIRunner(ctx, append(append([]string(nil), baseArgs...), "--plan"), token)
	if err != nil {
		return nil, fmt.Errorf("integrated setup plan failed: %w", err)
	}
	planSum := sha256.Sum256(planBytes)
	planSHA256 := hex.EncodeToString(planSum[:])
	if request.ExpectedPlanSHA256 == "" {
		return map[string]any{
			"schema_version": "hive.dashboard-integrated-setup-plan.v1",
			"plan_sha256":    planSHA256,
			"result":         plan,
		}, nil
	}
	if request.ExpectedPlanSHA256 != planSHA256 {
		return nil, fmt.Errorf("integrated setup plan changed: expected %s, current %s", request.ExpectedPlanSHA256, planSHA256)
	}
	applied, _, err := dashboardSetupCLIRunner(ctx, baseArgs, token)
	if err != nil {
		return nil, fmt.Errorf("integrated setup apply failed: %w", err)
	}
	return map[string]any{
		"schema_version": "hive.dashboard-integrated-setup-apply.v1",
		"plan_sha256":    planSHA256,
		"result":         applied,
	}, nil
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

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/dashboard"
)

func TestDashboardSetupEnvironmentReplacesOnlyGitHubToken(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "stale-token")
	t.Setenv("HIVE_SETUP_TEST_KEEP", "kept")
	environment := dashboardSetupEnvironment("fresh-token")
	var tokens, kept int
	for _, value := range environment {
		switch {
		case strings.HasPrefix(strings.ToUpper(value), "HIVE_GITHUB_TOKEN="):
			tokens++
			if value != "HIVE_GITHUB_TOKEN=fresh-token" {
				t.Fatalf("unexpected token environment entry")
			}
		case value == "HIVE_SETUP_TEST_KEEP=kept":
			kept++
		}
	}
	if tokens != 1 || kept != 1 {
		t.Fatalf("environment tokens=%d kept=%d", tokens, kept)
	}
	if os.Getenv("HIVE_GITHUB_TOKEN") != "stale-token" {
		t.Fatal("building the child environment changed the parent token")
	}
}

func TestDashboardIntegratedSetupBindsApplyToFreshPlan(t *testing.T) {
	original := dashboardSetupCLIRunner
	t.Cleanup(func() { dashboardSetupCLIRunner = original })
	stateDir := filepath.Join(t.TempDir(), "repository-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	runtimeManager, factoryCalls, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	originalRuntime := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(runtimeManager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(originalRuntime) })
	planBytes := []byte("{\"applied\":false}\n")
	var calls [][]string
	dashboardSetupCLIRunner = func(_ context.Context, args []string, token string) (map[string]any, []byte, error) {
		if token != "owner-token" {
			t.Fatalf("runner received unexpected token")
		}
		calls = append(calls, append([]string(nil), args...))
		if args[len(args)-1] == "--plan" {
			return map[string]any{"applied": false}, planBytes, nil
		}
		return map[string]any{"applied": true}, []byte("{\"applied\":true}\n"), nil
	}

	maxActiveIssues := 3
	request := dashboard.IntegratedSetupRequest{
		RequestID:  "setup-cycle-a-001",
		Repository: "owner/repository", Coverage: "essential", Automation: "repair-pr",
		Provider: "codex", VisualHiveRef: strings.Repeat("a", 40), MaxActiveIssues: &maxActiveIssues,
	}
	planDigest, err := dashboardSetupPlanDigest(request, planBytes)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedPlanSHA256 = planDigest
	result, err := runDashboardIntegratedSetup(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	activation, _ := result["visual_runtime_activation"].(map[string]any)
	if result["plan_sha256"] != planDigest || activation["state"] != "ready" || len(calls) != 2 {
		t.Fatalf("result=%v calls=%v", result, calls)
	}
	wantBase := []string{
		"setup", "--json", "--repo", "owner/repository", "--coverage", "essential",
		"--automation", "repair-pr", "--provider", "codex", "--runtime", "local",
		"--visual-hive=true", "--visual-hive-ref", strings.Repeat("a", 40),
		"--max-active-issues", "3",
	}
	if !reflect.DeepEqual(calls[0], append(append([]string(nil), wantBase...), "--plan")) ||
		!reflect.DeepEqual(calls[1], wantBase) {
		t.Fatalf("unexpected setup arguments: %v", calls)
	}
	replay, err := runDashboardIntegratedSetup(context.Background(), request, "owner-token")
	replayActivation, _ := replay["visual_runtime_activation"].(map[string]any)
	if err != nil || replay["idempotent_replay"] != true || replayActivation["state"] != "ready" ||
		len(calls) != 2 || factoryCalls.Load() != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, calls)
	}
}

func TestDashboardSetupPlanDigestIgnoresOnlyGeneratedAt(t *testing.T) {
	request := dashboard.IntegratedSetupRequest{
		RequestID: "setup-cycle-a-003", Repository: "owner/repository",
		Coverage: "comprehensive", Automation: "repair-pr", Provider: "codex",
		VisualHiveRef: strings.Repeat("b", 40),
	}
	first := []byte(`{"applied":false,"plan":{"generated_at":"2026-07-25T00:00:00Z","repository":"owner/repository","inspection":{"test_commands":[["npm","test"]]}}}`)
	second := []byte(`{
		"plan": {
			"inspection": {"test_commands": [["npm", "test"]]},
			"repository": "owner/repository",
			"generated_at": "2026-07-25T00:00:05Z"
		},
		"applied": false
	}`)
	firstDigest, err := dashboardSetupPlanDigest(request, first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := dashboardSetupPlanDigest(request, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("generated_at changed digest: first=%s second=%s", firstDigest, secondDigest)
	}

	changed := []byte(`{"applied":false,"plan":{"generated_at":"2026-07-25T00:00:05Z","repository":"owner/repository","inspection":{"test_commands":[["npm","run","build"]]}}}`)
	changedDigest, err := dashboardSetupPlanDigest(request, changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("a semantic setup-plan change did not change the digest")
	}

	changedLimit := request
	maxActiveIssues := 3
	changedLimit.MaxActiveIssues = &maxActiveIssues
	changedLimitDigest, err := dashboardSetupPlanDigest(changedLimit, first)
	if err != nil {
		t.Fatal(err)
	}
	if changedLimitDigest == firstDigest {
		t.Fatal("changing max_active_issues did not change the dashboard setup plan digest")
	}
}

func TestDashboardSetupPlanDigestRejectsInvalidOrTrailingJSON(t *testing.T) {
	request := dashboard.IntegratedSetupRequest{
		RequestID: "setup-cycle-a-004", Repository: "owner/repository",
		Coverage: "essential", Automation: "repair-pr", Provider: "codex",
		VisualHiveRef: strings.Repeat("c", 40),
	}
	for _, input := range [][]byte{
		[]byte(`{"plan":`),
		[]byte(`{"plan":{}} {"extra":true}`),
	} {
		if _, err := dashboardSetupPlanDigest(request, input); err == nil {
			t.Fatalf("expected invalid setup plan to fail: %q", input)
		}
	}
}

func TestDashboardIntegratedSetupRejectsPlanDriftBeforeApply(t *testing.T) {
	original := dashboardSetupCLIRunner
	t.Cleanup(func() { dashboardSetupCLIRunner = original })
	stateDir := filepath.Join(t.TempDir(), "repository-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	runtimeManager, factoryCalls, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		return &fakeNormalVisualRuntime{binding: binding}
	})
	originalRuntime := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(runtimeManager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(originalRuntime) })
	calls := 0
	dashboardSetupCLIRunner = func(context.Context, []string, string) (map[string]any, []byte, error) {
		calls++
		return map[string]any{"applied": false}, []byte("{\"changed\":true}\n"), nil
	}
	_, err := runDashboardIntegratedSetup(context.Background(), dashboard.IntegratedSetupRequest{
		RequestID:  "setup-cycle-a-002",
		Repository: "owner/repository", Coverage: "essential", Automation: "repair-pr",
		Provider: "codex", VisualHiveRef: strings.Repeat("a", 40), ExpectedPlanSHA256: strings.Repeat("0", 64),
	}, "owner-token")
	if err == nil || !strings.Contains(err.Error(), "plan changed") || calls != 1 || factoryCalls.Load() != 0 {
		t.Fatalf("err=%v calls=%d factory=%d", err, calls, factoryCalls.Load())
	}
}

func TestDashboardIntegratedSetupPersistsSuccessAndRetriesOnlyRuntimeActivation(t *testing.T) {
	originalRunner := dashboardSetupCLIRunner
	t.Cleanup(func() { dashboardSetupCLIRunner = originalRunner })
	stateDir := filepath.Join(t.TempDir(), "repository-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)
	contract := &testNormalVisualContract{config: testInstalledNormalVisualContract(t, stateDir), exists: true}
	var runtimeAttempts int
	runtimeManager, factoryCalls, _ := newTestNormalVisualRuntimeManager(t, contract, func(binding normalVisualRuntimeBinding) *fakeNormalVisualRuntime {
		runtimeAttempts++
		instance := &fakeNormalVisualRuntime{binding: binding}
		if runtimeAttempts == 1 {
			instance.startErr = errors.New("dashboard readiness is not yet available")
		}
		return instance
	})
	originalRuntime := dashboardNormalVisualRuntime.Load()
	dashboardNormalVisualRuntime.Store(runtimeManager)
	t.Cleanup(func() { dashboardNormalVisualRuntime.Store(originalRuntime) })

	planBytes := []byte("{\"applied\":false}\n")
	calls := 0
	dashboardSetupCLIRunner = func(_ context.Context, args []string, _ string) (map[string]any, []byte, error) {
		calls++
		if args[len(args)-1] == "--plan" {
			return map[string]any{"applied": false}, planBytes, nil
		}
		return map[string]any{"applied": true}, []byte("{\"applied\":true}\n"), nil
	}
	request := dashboard.IntegratedSetupRequest{
		RequestID: "setup-runtime-retry-001", Repository: "owner/repository",
		Coverage: "essential", Automation: "repair-pr", Provider: "codex",
		VisualHiveRef: strings.Repeat("a", 40),
	}
	planDigest, err := dashboardSetupPlanDigest(request, planBytes)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedPlanSHA256 = planDigest
	first, err := runDashboardIntegratedSetup(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	firstActivation, _ := first["visual_runtime_activation"].(map[string]any)
	if firstActivation["state"] != "pending" || firstActivation["error_reference"] == "" {
		t.Fatalf("first activation=%v", firstActivation)
	}
	replay, err := runDashboardIntegratedSetup(context.Background(), request, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	replayActivation, _ := replay["visual_runtime_activation"].(map[string]any)
	if replay["idempotent_replay"] != true || replayActivation["state"] != "ready" ||
		calls != 2 || factoryCalls.Load() != 2 {
		t.Fatalf("replay=%v calls=%d factory=%d", replay, calls, factoryCalls.Load())
	}
}

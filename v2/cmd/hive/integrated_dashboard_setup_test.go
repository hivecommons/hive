package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
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
	planBytes := []byte("{\"applied\":false}\n")
	planSum := sha256.Sum256(planBytes)
	planDigest := hex.EncodeToString(planSum[:])
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

	result, err := runDashboardIntegratedSetup(context.Background(), dashboard.IntegratedSetupRequest{
		Repository: "owner/repository", Coverage: "essential", Automation: "repair-pr",
		Provider: "codex", VisualHiveRef: strings.Repeat("a", 40), ExpectedPlanSHA256: planDigest,
	}, "owner-token")
	if err != nil {
		t.Fatal(err)
	}
	if result["plan_sha256"] != planDigest || len(calls) != 2 {
		t.Fatalf("result=%v calls=%v", result, calls)
	}
	wantBase := []string{
		"setup", "--json", "--repo", "owner/repository", "--coverage", "essential",
		"--automation", "repair-pr", "--provider", "codex", "--runtime", "local",
		"--visual-hive=true", "--visual-hive-ref", strings.Repeat("a", 40),
	}
	if !reflect.DeepEqual(calls[0], append(append([]string(nil), wantBase...), "--plan")) ||
		!reflect.DeepEqual(calls[1], wantBase) {
		t.Fatalf("unexpected setup arguments: %v", calls)
	}
}

func TestDashboardIntegratedSetupRejectsPlanDriftBeforeApply(t *testing.T) {
	original := dashboardSetupCLIRunner
	t.Cleanup(func() { dashboardSetupCLIRunner = original })
	calls := 0
	dashboardSetupCLIRunner = func(context.Context, []string, string) (map[string]any, []byte, error) {
		calls++
		return map[string]any{"applied": false}, []byte("{\"changed\":true}\n"), nil
	}
	_, err := runDashboardIntegratedSetup(context.Background(), dashboard.IntegratedSetupRequest{
		Repository: "owner/repository", Coverage: "essential", Automation: "repair-pr",
		Provider: "codex", VisualHiveRef: strings.Repeat("a", 40), ExpectedPlanSHA256: strings.Repeat("0", 64),
	}, "owner-token")
	if err == nil || !strings.Contains(err.Error(), "plan changed") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

package visualhive

import (
	"strings"
	"testing"
)

func TestResolveSpecialistRoutesVerifiedExactPairs(t *testing.T) {
	tests := []struct {
		name  string
		input SpecialistRoutingInput
		role  string
	}{
		{name: "normalized exact quality producer", input: SpecialistRoutingInput{IssueKind: " test_adequacy_gap ", OwningAgentHint: " VISUAL_HIVE/TEST_CREATOR "}, role: SpecialistQuality},
		{name: "mutation producer", input: SpecialistRoutingInput{IssueKind: "mutation_survivor", OwningAgentHint: "visual-hive/mutation"}, role: SpecialistQuality},
		{name: "test creator", input: SpecialistRoutingInput{IssueKind: "test_adequacy_gap", OwningAgentHint: "visual-hive/test-creator"}, role: SpecialistQuality},
		{name: "test maintainer", input: SpecialistRoutingInput{IssueKind: "weak_visual_test", OwningAgentHint: "visual-hive/test-maintainer"}, role: SpecialistQuality},
		{name: "coverage creator", input: SpecialistRoutingInput{IssueKind: "missing_visual_coverage", OwningAgentHint: "visual-hive/test-creator"}, role: SpecialistQuality},
		{name: "coverage map", input: SpecialistRoutingInput{IssueKind: "missing_visual_coverage", OwningAgentHint: "visual-hive/map"}, role: SpecialistScanner},
		{name: "workflow", input: SpecialistRoutingInput{IssueKind: "workflow_safety", OwningAgentHint: "hive/ci"}, role: SpecialistCIMaintainer},
		{name: "normalized ci producer", input: SpecialistRoutingInput{IssueKind: "build_failure", OwningAgentHint: "HIVE/CI"}, role: SpecialistCIMaintainer},
		{name: "repository test", input: SpecialistRoutingInput{IssueKind: RepositoryTestFailureKind, OwningAgentHint: "hive/test-repair"}, role: SpecialistCIMaintainer},
		{name: "protected target", input: SpecialistRoutingInput{IssueKind: "protected_target_blocked", OwningAgentHint: "hive/ci"}, role: SpecialistCIMaintainer},
		{name: "setup", input: SpecialistRoutingInput{IssueKind: "setup_needed", OwningAgentHint: "visual-hive/setup"}, role: SpecialistCIMaintainer},
		{name: "map", input: SpecialistRoutingInput{IssueKind: "map_drift", OwningAgentHint: "visual-hive/map"}, role: SpecialistScanner},
		{name: "onboarding setup", input: SpecialistRoutingInput{IssueKind: "external_repo_onboarding", OwningAgentHint: "visual-hive/setup"}, role: SpecialistCIMaintainer},
		{name: "onboarding repair", input: SpecialistRoutingInput{IssueKind: "external_repo_onboarding", OwningAgentHint: "hive/quality"}, role: SpecialistQuality},
		{name: "governance", input: SpecialistRoutingInput{IssueKind: "provider_governance", OwningAgentHint: "hive/ci"}, role: SpecialistCIMaintainer},
		{name: "future security exact pair", input: SpecialistRoutingInput{IssueKind: "security_finding", OwningAgentHint: "hive/sec-check"}, role: SpecialistSecurity},
		{name: "future architecture exact pair", input: SpecialistRoutingInput{IssueKind: "architecture_finding", OwningAgentHint: "hive/architect"}, role: SpecialistArchitect},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveSpecialist(test.input)
			if !got.DispatchAllowed || got.Role != test.role || strings.TrimSpace(got.Reason) == "" {
				t.Fatalf("ResolveSpecialist(%+v) = %+v, want allowed role %q with reason", test.input, got, test.role)
			}
		})
	}
}

func TestResolveSpecialistVisualRepairsRequireGroundedScope(t *testing.T) {
	for _, kind := range []string{"visual_regression", "selector_contract_failure", "screenshot_diff"} {
		hint := "hive/quality"
		if kind == "selector_contract_failure" {
			hint = "visual-hive/test-maintainer"
		}
		t.Run(kind+" grounded", func(t *testing.T) {
			got := ResolveSpecialist(SpecialistRoutingInput{IssueKind: kind, OwningAgentHint: hint, GroundedRepairScope: true})
			if !got.DispatchAllowed || got.Role != SpecialistQuality {
				t.Fatalf("grounded visual resolution = %+v, want quality dispatch", got)
			}
		})
		t.Run(kind+" ungrounded", func(t *testing.T) {
			got := ResolveSpecialist(SpecialistRoutingInput{IssueKind: kind, OwningAgentHint: hint})
			if !got.DispatchAllowed || got.Role != SpecialistScanner || !strings.Contains(strings.ToLower(got.Reason), "grounded") {
				t.Fatalf("ungrounded visual resolution = %+v, want scanner-only triage", got)
			}
		})
	}
}

func TestResolveSpecialistKeepsBaselineAuthorityHumanControlled(t *testing.T) {
	for _, kind := range []string{"stale_baseline", "missing_baseline", "baseline_approval", "baseline_approval_required"} {
		t.Run(kind, func(t *testing.T) {
			got := ResolveSpecialist(SpecialistRoutingInput{IssueKind: kind, OwningAgentHint: "hive/quality", GroundedRepairScope: true, BaselineChangesForbidden: true})
			assertSpecialistHold(t, got)
		})
	}

	missingBaselineScreenshot := ResolveSpecialist(SpecialistRoutingInput{IssueKind: "screenshot_diff", OwningAgentHint: "visual-hive/test-maintainer", GroundedRepairScope: true})
	assertSpecialistHold(t, missingBaselineScreenshot)

	churnWithoutContract := ResolveSpecialist(SpecialistRoutingInput{IssueKind: "baseline_churn", OwningAgentHint: "visual-hive/test-maintainer"})
	assertSpecialistHold(t, churnWithoutContract)

	churnDiagnosis := ResolveSpecialist(SpecialistRoutingInput{IssueKind: "baseline_churn", OwningAgentHint: "visual-hive/test-maintainer", BaselineChangesForbidden: true})
	if !churnDiagnosis.DispatchAllowed || churnDiagnosis.Role != SpecialistQuality || !strings.Contains(strings.ToLower(churnDiagnosis.Reason), "forbidding") {
		t.Fatalf("bounded baseline-churn diagnosis = %+v, want quality diagnosis with no baseline authority", churnDiagnosis)
	}
}

func TestResolveSpecialistRejectsUnknownOrMismatchedPairs(t *testing.T) {
	tests := []SpecialistRoutingInput{
		{IssueKind: "unknown_future_kind", OwningAgentHint: "hive/scanner"},
		{IssueKind: "mutation_survivor", OwningAgentHint: "hive/sec-check"},
		{IssueKind: "workflow_safety", OwningAgentHint: "hive/quality"},
		{IssueKind: "security issue hidden in prose", OwningAgentHint: "hive/sec-check"},
		{IssueKind: "architecture refactor request", OwningAgentHint: "hive/architect"},
		{IssueKind: "test_adequacy_gap", OwningAgentHint: "untrusted/quality"},
		{IssueKind: "test_adequacy_gap", OwningAgentHint: "hive/quality"},
		{IssueKind: "visual_regression", OwningAgentHint: "hive/sec-check", GroundedRepairScope: true},
	}
	for _, input := range tests {
		got := ResolveSpecialist(input)
		assertSpecialistHold(t, got)
		if !strings.Contains(strings.ToLower(got.Reason), "manual") {
			t.Fatalf("rejected resolution reason %q does not require manual review", got.Reason)
		}
	}
}

func assertSpecialistHold(t *testing.T, got SpecialistResolution) {
	t.Helper()
	if got.DispatchAllowed || got.Role != "" || strings.TrimSpace(got.Reason) == "" {
		t.Fatalf("resolution = %+v, want fail-closed manual hold", got)
	}
}

package repair

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/visualhive"
)

func TestLoadEvidenceSummaryClassifiesMissingRepairSignal(t *testing.T) {
	root := t.TempDir()
	verdict := `{"schemaVersion":"visual-hive.verdict.v1","allContributions":[
{"source":"workflow_audit","kind":"workflow_safety","status":"failed","gating":false,"contractId":"workflow-safety","reason":"Scheduled workflow does not publish a reviewed baseline manifest.","key":"workflow_audit.workflow_safety"}
]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		Title: "Scheduled workflow should publish a baseline manifest", AffectedContracts: []string{"workflow-safety"},
	})
	if !errors.Is(err, ErrNoActionableEvidence) {
		t.Fatalf("expected missing repair signal classification, got %v", err)
	}
}

func TestLoadEvidenceSummaryFiltersToFindingSignalAndContracts(t *testing.T) {
	root := t.TempDir()
	verdict := `{"schemaVersion":"visual-hive.verdict.v1","allContributions":[
{"source":"screenshot_diff","kind":"missing_baseline","status":"blocked","gating":true,"contractId":"deploy-preview-smoke","targetId":"deployPreview","reason":"review baseline","key":"screenshot_diff.missing_baseline.deploy-preview-smoke"},
{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"deploy-preview-smoke","targetId":"deployPreview","reason":"Failed to load resource: 404","key":"playwright.console_error.deploy-preview-smoke"},
{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"another-contract","targetId":"other","reason":"unrelated","key":"playwright.console_error.another-contract"}
]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		Title: "Repair deploy-preview-smoke: console_error", AffectedContracts: []string{"deploy-preview-smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "playwright.console_error.deploy-preview-smoke") || strings.Contains(summary, "missing_baseline") || strings.Contains(summary, "another-contract") {
		t.Fatalf("unexpected evidence summary: %s", summary)
	}
}

func TestLoadEvidenceSummaryBindsExactRepositoryTestFailure(t *testing.T) {
	root := t.TempDir()
	failed, err := visualhive.NewRepositoryTestResult("npm --prefix dashboard run test:all", 1)
	if err != nil {
		t.Fatal(err)
	}
	finding := visualhive.FindingLifecycle{
		Fingerprint: "repository-test:" + failed.Digest, RootCauseKey: "repository-test-failure/" + failed.Digest,
		IssueKind: visualhive.RepositoryTestFailureKind, ValidationCommand: failed.Command, AffectedContracts: []string{failed.Contract},
	}
	summary, err := LoadEvidenceSummary(root, finding)
	if err != nil || !strings.Contains(summary, "source=github_actions_job_step") || !strings.Contains(summary, "exit=nonzero") || !strings.Contains(summary, failed.Command) {
		t.Fatalf("exact repository test evidence was not summarized: %q err=%v", summary, err)
	}
	if err := os.WriteFile(filepath.Join(root, "repository-tests.tsv"), []byte("0\tnpm --prefix dashboard run test:all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if summaryAfterTamper, err := LoadEvidenceSummary(root, finding); err != nil || summaryAfterTamper != summary {
		t.Fatalf("target-writable ledger influenced runner-owned repair evidence: before=%q after=%q err=%v", summary, summaryAfterTamper, err)
	}
	finding.ValidationCommand = "python -m pytest -q"
	if _, err := LoadEvidenceSummary(root, finding); !errors.Is(err, ErrNoActionableEvidence) {
		t.Fatalf("mismatched repository command was accepted: %v", err)
	}
	finding.ValidationCommand = failed.Command
	finding.RootCauseKey += "-tampered"
	if _, err := LoadEvidenceSummary(root, finding); !errors.Is(err, ErrNoActionableEvidence) {
		t.Fatalf("tampered repository finding identity was accepted: %v", err)
	}
}

func TestLoadEvidenceSummaryUsesCanonicalAPI500Contribution(t *testing.T) {
	root := t.TempDir()
	verdict := `{"schemaVersion":"visual-hive.verdict.v1","allContributions":[
{"source":"mutation","kind":"mutation_adequacy","status":"failed","gating":true,"reason":"Mutation score 0% is below minimum 80%.","key":"mutation.mutation_adequacy"},
{"source":"mutation","kind":"mutation_survivor","status":"failed","gating":true,"operator":"api-500","contractId":"dashboard-shell","targetId":"localPreview","reason":"api-500 survived selected contracts. Safe repository-scoped repair: update contract \"dashboard-shell\" in visual-hive.config.yaml so selectors.textMustNotExist includes the exact first-party marker \"visual-hive api-500 mutation\"; do not invent or replace this marker.","key":"mutation.mutation_survivor.api-500"},
{"source":"mutation","kind":"mutation_survivor","status":"failed","gating":true,"operator":"empty-data","contractId":"dashboard-shell","targetId":"localPreview","reason":"unrelated survivor","key":"mutation.mutation_survivor.empty-data"}
]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "mutation_survivor", RootCauseKey: "mutation/api%2D500/localPreview/dashboard-shell",
		Title: "[Visual Hive] Mutation survived: api-500", AffectedContracts: []string{"dashboard-shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "mutation.mutation_survivor.api-500") ||
		!strings.Contains(summary, "visual-hive.config.yaml") ||
		!strings.Contains(summary, "visual-hive api-500 mutation") ||
		!strings.Contains(summary, "operator=api-500") ||
		strings.Contains(summary, "empty-data") || strings.Contains(summary, "unrelated") {
		t.Fatalf("unexpected canonical api-500 evidence summary: %s", summary)
	}

	legacySummary, legacyErr := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "mutation_survivor", Title: "[Visual Hive] Mutation survived: api-500", AffectedContracts: []string{"dashboard-shell"},
	})
	if legacyErr != nil || legacySummary != summary {
		t.Fatalf("strict legacy title binding changed the exact summary: summary=%q err=%v", legacySummary, legacyErr)
	}
}

func TestLoadEvidenceSummaryRejectsUnboundMutationContribution(t *testing.T) {
	valid := `{"allContributions":[{"source":"mutation","kind":"mutation_survivor","status":"failed","gating":true,"operator":"api-500","contractId":"dashboard-shell","targetId":"localPreview","reason":"exact guidance","key":"mutation.mutation_survivor.api-500"}]}`
	for _, test := range []struct {
		name    string
		verdict string
		finding visualhive.FindingLifecycle
	}{
		{name: "missing operator", verdict: strings.Replace(valid, `"operator":"api-500",`, "", 1)},
		{name: "wrong key", verdict: strings.Replace(valid, `mutation.mutation_survivor.api-500`, `mutation.mutation_survivor.empty-data`, 1)},
		{name: "wrong source", verdict: strings.Replace(valid, `"source":"mutation"`, `"source":"triage"`, 1)},
		{name: "wrong root operator", verdict: valid, finding: visualhive.FindingLifecycle{RootCauseKey: "mutation/empty-data/localPreview/dashboard-shell"}},
		{name: "wrong root target", verdict: valid, finding: visualhive.FindingLifecycle{RootCauseKey: "mutation/api-500/deployPreview/dashboard-shell"}},
		{name: "case changed root contract", verdict: valid, finding: visualhive.FindingLifecycle{RootCauseKey: "mutation/api-500/localPreview/Dashboard-shell"}},
		{name: "case changed contribution contract", verdict: strings.Replace(valid, `"contractId":"dashboard-shell"`, `"contractId":"Dashboard-shell"`, 1)},
		{name: "root and finding contract mismatch", verdict: valid, finding: visualhive.FindingLifecycle{RootCauseKey: "mutation/api-500/localPreview/other-shell"}},
		{name: "invalid legacy title", verdict: valid, finding: visualhive.FindingLifecycle{Title: "Mutation needs review: api-500"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(test.verdict), 0o600); err != nil {
				t.Fatal(err)
			}
			finding := test.finding
			finding.IssueKind = "mutation_survivor"
			finding.AffectedContracts = []string{"dashboard-shell"}
			if finding.RootCauseKey == "" && finding.Title == "" {
				finding.RootCauseKey = "mutation/api-500/localPreview/dashboard-shell"
			}
			if _, err := LoadEvidenceSummary(root, finding); !errors.Is(err, ErrNoActionableEvidence) {
				t.Fatalf("unbound mutation contribution was repairable: %v", err)
			}
		})
	}
}

func TestLoadEvidenceSummaryEscapesMutationControlCharacters(t *testing.T) {
	root := t.TempDir()
	verdict := `{"allContributions":[{"source":"mutation","kind":"mutation_survivor","status":"failed","gating":true,"operator":"api-500","contractId":"dash\nboard","targetId":"local\tpreview","reason":"line one\r\nline two","key":"mutation.mutation_survivor.api-500"}]}`
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "mutation_survivor", Title: "[Visual Hive] Mutation survived: api-500",
		AffectedContracts: []string{"dash\nboard"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(summary, "\r\n\t") || !strings.Contains(summary, `contract=dash\nboard`) || !strings.Contains(summary, `target=local\tpreview`) || !strings.Contains(summary, `reason=line one\r\nline two`) {
		t.Fatalf("mutation evidence was not serialized as one safe line: %q", summary)
	}
}

func TestLoadEvidenceSummaryStripsPlaywrightANSIStyling(t *testing.T) {
	root := t.TempDir()
	reason := "Expected text to be absent: Failed to fetch\n\n" +
		"\x1b[2mexpect(\x1b[22m\x1b[31mlocator\x1b[39m) failed\n\n" +
		"Expected: \x1b[32m0\x1b[39m\nReceived: \x1b[31m1\x1b[39m"
	verdict, err := json.Marshal(verdictEvidence{AllContributions: []evidenceContribution{{
		Source: "playwright", Kind: "contract_result", Status: "failed", Gating: true,
		ContractID: "app-shell-content-health", TargetID: "localPreview", Reason: reason,
		Key: "playwright.contract_result.app-shell-content-health",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), verdict, 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "selector_contract_failure", Title: "[Visual Hive] app-shell-content-health failed deterministic validation",
		AffectedContracts: []string{"app-shell-content-health"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Expected: 0") || !strings.Contains(summary, "Received: 1") || !strings.Contains(summary, `\n`) ||
		strings.ContainsRune(summary, '\x1b') || strings.Contains(summary, "[32m") {
		t.Fatalf("Playwright evidence styling was not safely normalized: %q", summary)
	}

	for _, unsafe := range []string{"bell\x07", "clear\x1b[2J"} {
		if _, err := safeEvidenceValue(unsafe); err == nil || !strings.Contains(err.Error(), "unsafe control character") {
			t.Fatalf("non-SGR control %q did not remain fail-closed: %v", unsafe, err)
		}
	}
	styledSecret := "github_\x1b[31mpat_\x1b[0m" + strings.Repeat("a", 24)
	if _, err := safeEvidenceValue(styledSecret); err == nil || !strings.Contains(err.Error(), "github-token-v1") || strings.Contains(err.Error(), "github_pat_"+strings.Repeat("a", 24)) || strings.Contains(err.Error(), styledSecret) {
		t.Fatalf("ANSI-split credential was not rejected after normalization: %v", err)
	}
}

func TestLoadEvidenceSummaryRejectsCredentials(t *testing.T) {
	tests := []struct{ value, rule string }{
		{value: "github_" + "pat_" + strings.Repeat("a", 24), rule: "github-token-v1"},
		{value: "AK" + "IA" + strings.Repeat("A", 16), rule: "aws-access-key-id-v1"},
		{value: `clientSecret = "actual-operator-value-must-not-leave"`, rule: "sensitive-assignment-v2"},
	}
	for _, test := range tests {
		root := t.TempDir()
		verdict := `{"allContributions":[{"source":"playwright","kind":"console_error","status":"failed","gating":true,"contractId":"app","reason":` + fmt.Sprintf("%q", test.value) + `,"key":"playwright.console_error.app"}]}`
		if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(verdict), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{Title: "console_error", AffectedContracts: []string{"app"}})
		if err == nil || !strings.Contains(err.Error(), test.rule) || strings.Contains(err.Error(), test.value) {
			t.Fatalf("expected non-disclosing credential rejection for %s, got %v", test.rule, err)
		}
	}
}

func TestLoadEvidenceSummaryUsesDeterministicCoverageRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"allContributions":[{"source":"playwright","kind":"deterministic_run","status":"passed","gating":true,"reason":"passed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	coverage := `{"maintenanceFindings":[
{"id":"missing-mobile-scenarios","kind":"missing_mobile_viewport","contractId":"scenario-gallery-contract","targetId":"localPreview","route":"/scenarios","viewport":"mobile","message":"Contract has no mobile screenshot.","evidence":["viewports=desktop"],"recommendedAction":"expand"},
{"id":"unrelated","kind":"missing_mobile_viewport","contractId":"other","message":"unrelated"}
],"recommendations":[{"maintenanceFindingId":"missing-mobile-scenarios","suggestedConfigYaml":"screenshots:\n  - name: scenarios-mobile\n    route: /scenarios\n    viewport: mobile"}]}`
	if err := os.WriteFile(filepath.Join(root, "coverage-recommendations.json"), []byte(coverage), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "missing_visual_coverage", Title: "Maintain visual test: missing_mobile_viewport", AffectedContracts: []string{"scenario-gallery-contract"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "coverage.missing-mobile-scenarios") || !strings.Contains(summary, "suggested_config=screenshots") || strings.Contains(summary, "unrelated") {
		t.Fatalf("unexpected coverage evidence summary: %s", summary)
	}
}

func TestLoadEvidenceSummaryUsesFirstClassFlowRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"allContributions":[{"source":"playwright","kind":"deterministic_run","status":"passed","gating":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	coverage := `{"maintenanceFindings":[],"recommendations":[
{"id":"flow-steps:app-shell-stability","kind":"add_flow_steps","title":"Add deterministic flow steps for \"app-shell-stability\"","contractId":"app-shell-stability","targetId":"localPreview","route":"/","rationale":["Contract has no deterministic user-flow steps."],"suggestedConfigYaml":"steps:\n  - action: goto\n    route: /\n  - action: assertVisible\n    selector: '[data-testid=dashboard-page]'","suggestedTests":["Run the contract locally."]},
{"id":"flow-steps:other","kind":"add_flow_steps","title":"Add deterministic flow steps for other","contractId":"other"}
]}`
	if err := os.WriteFile(filepath.Join(root, "coverage-recommendations.json"), []byte(coverage), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "missing_visual_coverage", Title: `[Visual Hive] Add visual coverage: Add deterministic flow steps for "app-shell-stability"`, AffectedContracts: []string{"app-shell-stability"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "coverage.flow-steps:app-shell-stability") || !strings.Contains(summary, "kind=add_flow_steps") || !strings.Contains(summary, "assertVisible") || strings.Contains(summary, "flow-steps:other") {
		t.Fatalf("unexpected first-class coverage evidence: %s", summary)
	}
}

func TestLoadEvidenceSummaryUsesVerifiedTestCreationRecommendation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unresolved := strictTestCreationRecommendation()
	unresolved["id"] = "unresolved-layer-2"
	unresolved["rationale"] = []string{"unresolved-sentinel"}
	unresolved["suggestedTests"] = []string{"invented-edit-sentinel"}
	unresolved["grounding"] = map[string]interface{}{"status": "unresolved", "evidence": []string{}, "unresolvedReasons": []string{"Repository evidence is incomplete."}}
	unresolved["suggestedContract"] = map[string]interface{}{"id": "unresolved-layer-2", "description": "Resolve repository mapping", "selectors": []string{}, "mustNotExistSelectors": []string{}, "textMustExist": []string{}, "textMustNotExist": []string{}, "maskSelectors": []string{}}

	grounded := strictTestCreationRecommendation()
	grounded["id"] = "layer-2-unknown"
	grounded["rationale"] = []string{"No repository unit test was detected."}
	grounded["suggestedTests"] = []string{"Add focused tests for non-visual behavior."}
	grounded["artifacts"] = []string{".visual-hive/repo-map.json"}
	grounded["grounding"] = map[string]interface{}{"status": "grounded", "evidence": []string{".visual-hive/repo-map.json"}, "unresolvedReasons": []string{}}

	unrelated := strictTestCreationRecommendation()
	unrelated["id"] = "layer-3-unknown"
	unrelated["gapId"] = "testing-layer:3:unknown"
	unrelated["kind"] = "accessibility_check"
	unrelated["title"] = "unrelated"
	if err := os.WriteFile(filepath.Join(root, "test-creation-plan.json"), strictTestCreationPlan(t, []map[string]interface{}{unresolved, grounded, unrelated}), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
		IssueKind: "test_adequacy_gap", Title: "[Visual Hive] Add repository test coverage: Add unit test evidence for Unit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "test_creation.layer-2-unknown") || !strings.Contains(summary, "grounding_evidence=.visual-hive/repo-map.json") || !strings.Contains(summary, "required_scope=test_files_only") || strings.Contains(summary, "unrelated") || strings.Contains(summary, "unresolved-sentinel") || strings.Contains(summary, "invented-edit-sentinel") {
		t.Fatalf("unexpected test-creation evidence: %s", summary)
	}
}

func TestLoadEvidenceSummaryRejectsLegacyUnresolvedAndMalformedTestCreationRecommendations(t *testing.T) {
	unresolved := strictTestCreationRecommendation()
	unresolved["id"] = "unresolved"
	unresolved["suggestedTests"] = []string{"unresolved-edit-sentinel"}
	unresolved["grounding"] = map[string]interface{}{"status": "unresolved", "evidence": []string{}, "unresolvedReasons": []string{"No repository mapping."}}
	unresolved["suggestedContract"] = map[string]interface{}{"id": "unresolved", "description": "Resolve repository mapping", "selectors": []string{}, "mustNotExistSelectors": []string{}, "textMustExist": []string{}, "textMustNotExist": []string{}, "maskSelectors": []string{}}

	missingGrounding := strictTestCreationRecommendation()
	missingGrounding["id"] = "missing-grounding"
	missingGrounding["suggestedTests"] = []string{"missing-grounding-edit-sentinel"}
	delete(missingGrounding, "grounding")

	missingEvidence := strictTestCreationRecommendation()
	missingEvidence["id"] = "missing-evidence"
	missingEvidence["suggestedTests"] = []string{"missing-evidence-edit-sentinel"}
	missingEvidence["grounding"] = map[string]interface{}{"status": "grounded", "evidence": []string{}, "unresolvedReasons": []string{}}

	unknownField := strictTestCreationRecommendation()
	unknownField["id"] = "unknown-field"
	unknownField["inventedScope"] = "src/**"

	validEmpty := strictTestCreationPlan(t, []map[string]interface{}{})
	trailing := append(append([]byte(nil), validEmpty...), []byte(`
{"suggestedTests":["trailing-edit-sentinel"]}`)...)
	cases := []struct {
		name               string
		plan               []byte
		expectNoActionable bool
	}{
		{
			name:               "legacy v1 cannot claim grounded authority",
			plan:               []byte(`{"schemaVersion":"visual-hive.test-creation-plan.v1","recommendations":[{"id":"legacy","suggestedTests":["legacy-edit-sentinel"]}]}`),
			expectNoActionable: true,
		},
		{
			name: "relabeled v1 subset is not strict v2",
			plan: []byte(`{"schemaVersion":"visual-hive.test-creation-plan.v2","recommendations":[{"id":"relabeled","gapId":"testing-layer:2:unknown","source":"testing_layer","kind":"unit_test","priority":"medium","title":"Add unit test evidence for Unit","rationale":[],"suggestedTests":["legacy-edit-sentinel"],"artifacts":[],"grounding":{"status":"grounded","evidence":["legacy"],"unresolvedReasons":[]}}]}`),
		},
		{
			name:               "unresolved v2 has no edit scope",
			plan:               strictTestCreationPlan(t, []map[string]interface{}{unresolved}),
			expectNoActionable: true,
		},
		{
			name: "missing grounding is malformed",
			plan: strictTestCreationPlan(t, []map[string]interface{}{missingGrounding}),
		},
		{
			name: "grounded without evidence is malformed",
			plan: strictTestCreationPlan(t, []map[string]interface{}{missingEvidence}),
		},
		{
			name: "unknown recommendation field is malformed",
			plan: strictTestCreationPlan(t, []map[string]interface{}{unknownField}),
		},
		{
			name: "trailing JSON is malformed",
			plan: trailing,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "verdict.json"), []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "test-creation-plan.json"), test.plan, 0o600); err != nil {
				t.Fatal(err)
			}
			summary, err := LoadEvidenceSummary(root, visualhive.FindingLifecycle{
				IssueKind: "test_adequacy_gap", Title: "[Visual Hive] Add repository test coverage: Add unit test evidence for Unit",
			})
			if err == nil || summary != "" {
				t.Fatalf("expected no repair scope, got summary=%q err=%v", summary, err)
			}
			if test.expectNoActionable && !errors.Is(err, ErrNoActionableEvidence) {
				t.Fatalf("expected no-actionable-evidence classification, got %v", err)
			}
			for _, sentinel := range []string{"legacy-edit-sentinel", "unresolved-edit-sentinel", "missing-grounding-edit-sentinel", "missing-evidence-edit-sentinel", "trailing-edit-sentinel"} {
				if strings.Contains(summary, sentinel) {
					t.Fatalf("rejected scope leaked %q: %s", sentinel, summary)
				}
			}
		})
	}
}

func strictTestCreationPlan(t *testing.T, recommendations []map[string]interface{}) []byte {
	t.Helper()
	summary := map[string]int{
		"total": len(recommendations), "high": 0, "medium": 0, "low": 0,
		"fromTestingLayers": 0, "fromCoverageRecommendations": 0, "fromMutationSurvivors": 0, "fromHandoffWorkItems": 0,
	}
	for _, recommendation := range recommendations {
		if priority, ok := recommendation["priority"].(string); ok {
			summary[priority]++
		}
		switch recommendation["source"] {
		case "testing_layer":
			summary["fromTestingLayers"]++
		case "coverage_recommendation":
			summary["fromCoverageRecommendations"]++
		case "mutation_survivor":
			summary["fromMutationSurvivors"]++
		case "handoff_work_item":
			summary["fromHandoffWorkItems"]++
		}
	}
	plan := map[string]interface{}{
		"schemaVersion": "visual-hive.test-creation-plan.v2",
		"generatedAt":   "2026-07-14T12:00:00.000Z",
		"project":       "demo",
		"sourceArtifacts": map[string]interface{}{
			"evidencePacket": ".visual-hive/evidence-packet.json",
		},
		"governance": map[string]interface{}{
			"verdictAuthority": "visual_hive", "agentAuthority": "advisory_test_generation_only",
			"writePolicy": "no_config_or_test_files_written", "secretPolicy": "redacted_values_names_only",
		},
		"summary":         summary,
		"recommendations": recommendations,
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func strictTestCreationRecommendation() map[string]interface{} {
	return map[string]interface{}{
		"id":       "layer-2-unit",
		"gapId":    "testing-layer:2:unknown",
		"source":   "testing_layer",
		"kind":     "unit_test",
		"priority": "medium",
		"title":    "Add unit test evidence for Unit",
		"rationale": []string{
			"No repository unit test was detected.",
		},
		"affected":        map[string]interface{}{"route": "/observed"},
		"currentEvidence": []string{"Artifact: .visual-hive/repo-map.json"},
		"grounding":       map[string]interface{}{"status": "grounded", "evidence": []string{"repoMap.node:unit"}, "unresolvedReasons": []string{}},
		"suggestedContract": map[string]interface{}{
			"id": "unit", "description": "Observed unit contract", "route": "/observed",
			"selectors": []string{"[data-testid='observed']"}, "mustNotExistSelectors": []string{},
			"textMustExist": []string{}, "textMustNotExist": []string{}, "maskSelectors": []string{},
		},
		"suggestedMutation": "not_applicable",
		"validationCommand": "go test ./...",
		"hiveOwner":         "tester",
		"layer":             map[string]interface{}{"id": 2, "name": "Unit", "status": "unknown"},
		"suggestedTests":    []string{"Add focused tests for non-visual behavior."},
		"artifacts":         []string{".visual-hive/repo-map.json"},
		"trustedOnly":       false,
		"applyMode":         "advisory_no_write",
	}
}

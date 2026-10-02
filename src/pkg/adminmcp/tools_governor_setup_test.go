package adminmcp

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// sampleGovernorSettings mirrors the decoded GET /api/config/governor fields
// the proposal reads (JSON numbers decode as float64).
func sampleGovernorSettings(repoCount float64, scaling string, effective map[string]any, pinned map[string]any, totalTokens float64) map[string]any {
	return map[string]any{
		"repoCount":           repoCount,
		"cadenceScope":        config.CadenceScopeAggregate,
		"thresholdScaling":    scaling,
		"effectiveThresholds": effective,
		"pinnedThresholds":    pinned,
		"budget":              map[string]any{"totalTokens": totalTokens, "periodDays": float64(7), "criticalPct": float64(90)},
	}
}

func governorSetupRecs(t *testing.T, out any) map[string]governorSetupRecommendation {
	t.Helper()
	body, ok := out.(map[string]any)
	if !ok || body["type"] != ToolGovernorSetup {
		t.Fatalf("result = %#v", out)
	}
	recs, ok := body["recommendations"].([]governorSetupRecommendation)
	if !ok || len(recs) != 3 {
		t.Fatalf("recommendations = %#v", body["recommendations"])
	}
	if recs[0].Setting != "threshold_scaling" || recs[1].Setting != "thresholds" {
		t.Fatalf("recommendations out of apply order: %+v", recs)
	}
	out2 := map[string]governorSetupRecommendation{}
	for _, r := range recs {
		if strings.TrimSpace(r.Reason) == "" {
			t.Fatalf("%s has no reason", r.Setting)
		}
		if _, ok := DefaultWriteRegistry().Get(r.Operation); !ok {
			t.Fatalf("%s names unregistered write operation %q", r.Setting, r.Operation)
		}
		out2[r.Setting] = r
	}
	return out2
}

// previewRecommendation proves a proposal entry is directly appliable: its
// args must pass the named write operation's own preview validation.
func previewRecommendation(t *testing.T, r governorSetupRecommendation) WritePreview {
	t.Helper()
	op, _ := DefaultWriteRegistry().Get(r.Operation)
	preview, err := op.Preview(context.Background(), r.Args)
	if err != nil {
		t.Fatalf("%s args %#v rejected by %s preview: %v", r.Setting, r.Args, r.Operation, err)
	}
	return preview
}

func TestGovernorSetupProposalKeepsAWellConfiguredSmallHive(t *testing.T) {
	data := sampleGovernorSettings(3, config.ThresholdScalingLinear,
		map[string]any{"quiet": float64(6), "busy": float64(30), "surge": float64(60)}, map[string]any{}, 1000000)
	out, err := GovernorSetupProposal(data)
	if err != nil {
		t.Fatal(err)
	}
	recs := governorSetupRecs(t, out)
	for setting, r := range recs {
		if r.Change {
			t.Fatalf("%s proposes a change on an already-sound hive: %+v", setting, r)
		}
	}
	if recs["thresholds"].Args != nil {
		t.Fatalf("unpinned thresholds must not propose explicit values: %#v", recs["thresholds"].Args)
	}
	if !strings.Contains(recs["thresholds"].Disclosure, "operator-owned") {
		t.Fatalf("thresholds disclosure = %q, want the #4037 scaling interaction", recs["thresholds"].Disclosure)
	}
	if !strings.Contains(out.(map[string]any)["summary"].(string), "nothing needs applying") {
		t.Fatalf("summary = %v", out.(map[string]any)["summary"])
	}
}

func TestGovernorSetupProposalRecommendsSqrtForManyRepos(t *testing.T) {
	data := sampleGovernorSettings(16, config.ThresholdScalingLinear,
		map[string]any{"quiet": float64(32), "busy": float64(160), "surge": float64(320)}, map[string]any{}, 1000000)
	out, err := GovernorSetupProposal(data)
	if err != nil {
		t.Fatal(err)
	}
	r := governorSetupRecs(t, out)["threshold_scaling"]
	if !r.Change || r.Recommended != config.ThresholdScalingSqrt || r.Args["scaling"] != config.ThresholdScalingSqrt {
		t.Fatalf("scaling = %+v", r)
	}
	if r.Disclosure == "" {
		t.Fatalf("a scaling change must carry its widening disclosure: %+v", r)
	}
	preview := previewRecommendation(t, r)
	if preview.Operation != WriteOpGovernorThresholdScaling {
		t.Fatalf("preview = %+v", preview)
	}
}

func TestGovernorSetupProposalRepinsDriftedThresholdsWithDisclosure(t *testing.T) {
	data := sampleGovernorSettings(4, config.ThresholdScalingLinear,
		map[string]any{"quiet": float64(1), "busy": float64(3), "surge": float64(5)},
		map[string]any{"quiet": true, "busy": true, "surge": true}, 1000000)
	out, err := GovernorSetupProposal(data)
	if err != nil {
		t.Fatal(err)
	}
	r := governorSetupRecs(t, out)["thresholds"]
	if !r.Change {
		t.Fatalf("thresholds = %+v, want a change", r)
	}
	want := map[string]any{"quiet": config.DefaultThresholdQuiet * 4, "busy": config.DefaultThresholdBusy * 4, "surge": config.DefaultThresholdSurge * 4}
	for mode, v := range want {
		if r.Args[mode] != v {
			t.Fatalf("args = %#v, want %#v", r.Args, want)
		}
	}
	if !strings.Contains(r.Disclosure, "scaling does not apply") {
		t.Fatalf("disclosure = %q, want the scaling interaction named", r.Disclosure)
	}
	previewRecommendation(t, r)
}

func TestGovernorSetupProposalAsksOwnerForMissingBudget(t *testing.T) {
	data := sampleGovernorSettings(1, config.ThresholdScalingLinear,
		map[string]any{"quiet": float64(2), "busy": float64(10), "surge": float64(20)}, map[string]any{}, 0)
	out, err := GovernorSetupProposal(data)
	if err != nil {
		t.Fatal(err)
	}
	r := governorSetupRecs(t, out)["budget"]
	if !r.Change || r.Operation != WriteOpBudgetUpdate || len(r.OwnerInput) != 1 || r.OwnerInput[0] != "totalTokens" {
		t.Fatalf("budget = %+v", r)
	}
}

func TestGovernorSetupProposalPerRepoCadenceKeepsScaling(t *testing.T) {
	data := sampleGovernorSettings(30, config.ThresholdScalingLinear,
		map[string]any{"quiet": float64(2), "busy": float64(10), "surge": float64(20)}, map[string]any{}, 1000000)
	data["cadenceScope"] = config.CadenceScopePerRepo
	out, err := GovernorSetupProposal(data)
	if err != nil {
		t.Fatal(err)
	}
	if r := governorSetupRecs(t, out)["threshold_scaling"]; r.Change {
		t.Fatalf("per_repo cadence must not propose a scaling change: %+v", r)
	}
}

func TestGovernorSetupProposalRejectsNonObject(t *testing.T) {
	if _, err := GovernorSetupProposal([]any{}); err == nil {
		t.Fatal("expected an error for a non-object governor settings response")
	}
}

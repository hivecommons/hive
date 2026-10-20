package stagelabels_test

import (
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestClaimLabelsMatchStages(t *testing.T) {
	if got := stagelabels.Claimed.Label(); got != claims.LabelClaimed {
		t.Fatalf("claims.LabelClaimed = %q, stage table says %q", claims.LabelClaimed, got)
	}
	if got := stagelabels.PRInProgress.Label(); got != claims.LabelPRInProgress {
		t.Fatalf("claims.LabelPRInProgress = %q, stage table says %q", claims.LabelPRInProgress, got)
	}
	for _, l := range []string{claims.LegacyLabelClaimed, claims.LegacyLabelInProgress, stagelabels.LegacyCoveredByPR, stagelabels.LegacyCoveredByPRASCII, "CLAIMED"} {
		if !stagelabels.IsLegacy(l) || !stagelabels.Owned(l) {
			t.Errorf("%q should be a retired, owned label", l)
		}
	}
	if stagelabels.IsLegacy(claims.LabelClaimed) || stagelabels.IsLegacy("hive/claimed-by-bob") {
		t.Fatal("live stage label / who-modifier must not be legacy")
	}
	if !stagelabels.CarriesStageAtLeast([]string{"bug", stagelabels.MergeQueue.Label()}, stagelabels.CoveredByPR) {
		t.Fatal("mirrored PR stage should count as covered")
	}
	if stagelabels.CarriesStageAtLeast([]string{stagelabels.Claimed.Label()}, stagelabels.CoveredByPR) || stagelabels.CarriesStageAtLeast(nil, stagelabels.Actionable) {
		t.Fatal("claimed / no stage must not count as covered")
	}
}

func TestNeedsDecisionLabelMatchesConfigDefault(t *testing.T) {
	if stagelabels.IssueNeedsDecision.Label() != config.DefaultNeedsDecisionLabel {
		t.Fatalf("stage label %q != config.DefaultNeedsDecisionLabel %q", stagelabels.IssueNeedsDecision.Label(), config.DefaultNeedsDecisionLabel)
	}
	if stagelabels.NeedsDecision.Label() != config.PRNeedsDecisionStageLabel {
		t.Fatalf("stage label %q != config.PRNeedsDecisionStageLabel %q", stagelabels.NeedsDecision.Label(), config.PRNeedsDecisionStageLabel)
	}
	if stagelabels.StageBySlugFor("needs-decision", stagelabels.IssueItem) != stagelabels.IssueNeedsDecision || stagelabels.StageBySlugFor("needs-decision", stagelabels.PRItem) != stagelabels.NeedsDecision {
		t.Fatal("StageBySlugFor must resolve the shared slug per kind")
	}
}

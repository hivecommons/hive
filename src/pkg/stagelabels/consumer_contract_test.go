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

// TestPossiblyExistingLabelMatchesConfig pins the prior-art park signal
// (hivecommons/hive#13575): config's hard-suppress reader and the stage
// table must spell stage/017 the same way, or a parked issue would be
// claimable.
func TestPossiblyExistingLabelMatchesConfig(t *testing.T) {
	if stagelabels.PossiblyExisting.Label() != config.PossiblyExistsStageLabel {
		t.Fatalf("stage label %q != config.PossiblyExistsStageLabel %q", stagelabels.PossiblyExisting.Label(), config.PossiblyExistsStageLabel)
	}
	if got := (config.IssueFilterConfig{}).HardSuppressIssueBucket([]string{"bug", stagelabels.PossiblyExisting.Label()}); got != config.PossiblyExistsStageLabel {
		t.Fatalf("HardSuppressIssueBucket = %q, want the 017 label", got)
	}
}

// TestPossiblyExistingStage: 017 is an issue-only parking bay between 015
// and 020; leaving it is never a regression.
func TestPossiblyExistingStage(t *testing.T) {
	s := stagelabels.PossiblyExisting
	if s.Number() != 17 || s.Slug() != "possible-existing" || s.String() != "017-possible-existing" {
		t.Fatalf("017 identity: %d %q %q", s.Number(), s.Slug(), s.String())
	}
	if !s.Applies(stagelabels.IssueItem) || s.Applies(stagelabels.PRItem) {
		t.Fatal("017 must be issue-only")
	}
	if !stagelabels.TriageNeeded.Before(s) || !s.Before(stagelabels.Claimed) {
		t.Fatal("017 sits between 015 and 020")
	}
	if s.Dwell() != 0 {
		t.Fatalf("017 is open-ended (parked on the author), dwell %v", s.Dwell())
	}
	for _, to := range []stagelabels.Stage{stagelabels.Actionable, stagelabels.TriageNeeded} {
		if stagelabels.IsRegression(s, to) {
			t.Errorf("017 → %v flagged as regression", to)
		}
	}
	if !stagelabels.IsRegression(stagelabels.Claimed, s) {
		t.Fatal("020 → 017 must be a regression, like → 010")
	}
}

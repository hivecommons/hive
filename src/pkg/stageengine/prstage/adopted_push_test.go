package prstage

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The one place that says which adoption escalations a maintainer's
// direction on the same head does not release (the relay then keeps the
// contributor's routing instead of promising the hive's fixer).
func TestAdoptionEscalationOutlastsDecision(t *testing.T) {
	for reason, want := range map[string]bool{
		"": false,
		stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:build":                        false,
		stagelabels.LaneReasonAdoptionStalledPrefix + "contributor-lane:review-requires-human":     false,
		stagelabels.LaneReasonAdoptionFixerReverts:                                                 true,
		stagelabels.LaneReasonAdoptionChildClosedTwice:                                             true,
		stagelabels.LaneReasonAdoptionCopyFailed:                                                   true,
		stagelabels.LaneReasonAdoptionStalledPrefix + ContributorLaneReasonWorkflowPath:            true,
		stagelabels.LaneReasonAdoptionStalledPrefix + AdoptStallForkRehomeRefused:                  true,
		stagelabels.LaneReasonAdoptionStalledPrefix + "supersede-failed:contributor-offline":       true,
		stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonHiveSignoffAttribution: true,
		stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice:      true,
		"some-future-escalation": true,
	} {
		if got := AdoptionEscalationOutlastsDecision(reason); got != want {
			t.Errorf("AdoptionEscalationOutlastsDecision(%q) = %v, want %v", reason, got, want)
		}
	}
}

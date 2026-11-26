package prstage

import (
	"strings"
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

// Each reason the hive cannot push an adopted PR's branch, in the order the
// dashboard's delegate and the PR-decision relay read them; a pushable
// branch has none.
func TestAdoptedPushFactsRefusal(t *testing.T) {
	stalled := stagelabels.LaneReasonAdoptionStalledPrefix
	for _, tc := range []struct {
		name  string
		facts AdoptedPushFacts
		want  string
	}{
		{"same-repo branch", AdoptedPushFacts{}, ""},
		{"fork with maintainer edits", AdoptedPushFacts{Fork: true, MaintainerCanModify: true}, ""},
		{"rehome refused", AdoptedPushFacts{Fork: true, MaintainerCanModify: true, RehomeRefused: true}, "fork-rehome-refused"},
		{"rehome-refused escalation", AdoptedPushFacts{Escalation: stalled + AdoptStallForkRehomeRefused}, "fork-rehome-refused"},
		{"fork without maintainer edits", AdoptedPushFacts{Fork: true}, "does not allow edits by maintainers"},
		{"fork changing workflows", AdoptedPushFacts{Fork: true, MaintainerCanModify: true, WorkflowPath: true}, "cannot push to a fork"},
		{"same-repo branch changing workflows", AdoptedPushFacts{WorkflowPath: true}, ""},
		{"workflow-path escalation", AdoptedPushFacts{Escalation: stalled + ContributorLaneReasonWorkflowPath}, "adoption stalled because the PR changes .github/workflows/"},
		{"capability blocker", AdoptedPushFacts{Capability: "App lacks workflows permission"}, "reported it cannot push: App lacks workflows permission"},
		{"idle escalation", AdoptedPushFacts{Escalation: stalled + "check-failing:build"}, ""},
	} {
		got := tc.facts.Refusal()
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: Refusal() = %q, want none", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: Refusal() = %q, want it to name %q", tc.name, got, tc.want)
		}
	}
}

// FromRecord reads the refused re-home, the escalation and whether the hive
// took the PR on for exactly the head asked about.
func TestAdoptedPushFactsFromRecord(t *testing.T) {
	const head, other = "aaa111", "bbb222"
	reason := stagelabels.LaneReasonAdoptionStalledPrefix + AdoptStallForkRehomeRefused
	rec := stagelabels.Record{ForkRehomeRefusedHead: head, AdoptEscalatedHead: head, AdoptEscalatedReason: " " + reason + " "}

	var f AdoptedPushFacts
	f.FromRecord(rec, head)
	if !f.RehomeRefused || f.Escalation != reason || !f.Adopted {
		t.Fatalf("FromRecord(head) = %+v, want refused, escalation %q, adopted", f, reason)
	}

	var g AdoptedPushFacts
	g.FromRecord(rec, other)
	if g.RehomeRefused || g.Escalation != "" || g.Adopted {
		t.Fatalf("FromRecord(other head) = %+v, want nothing recorded", g)
	}

	var h AdoptedPushFacts
	h.FromRecord(stagelabels.Record{AdoptedHead: other}, other)
	if !h.Adopted || h.RehomeRefused || h.Escalation != "" {
		t.Fatalf("FromRecord(live adoption) = %+v, want adopted only", h)
	}
}

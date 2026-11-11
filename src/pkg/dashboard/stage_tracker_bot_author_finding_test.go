package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Another bot's PR parked at 085 with the reviewer's finding: the popover
// asks where the fix lands, and Give direction (which goes to a fixer that
// never pushes there) and Approve (which does not clear the change request)
// are disabled with the reason.
func TestDecisionForPRBotAuthorFinding(t *testing.T) {
	it := StageTrackerItem{Repo: "acme/widget", Number: 679, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "e9e24fdabcdef", Reason: stagelabels.LaneReasonBotAuthorFinding}
	d := decisionForPR(it, &review.Aggregate{HumanSummaries: []string{"the build workflow drops the arm64 job"}}, "", false)
	if d == nil || d.AskedBy != decisionAskedByReviewer ||
		!strings.Contains(d.Question, "another bot opened and regenerates") ||
		!strings.Contains(d.Question, "the build workflow drops the arm64 job") ||
		!strings.HasSuffix(d.Question, "Fix it on the base branch, merge it as is, or close it?") {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Blocker, "no fix is pushed here") {
		t.Fatalf("blocker = %q", d.Blocker)
	}
	for _, o := range d.Options {
		switch o.Action {
		case decisionActionDirection:
			if o.Enabled || o.ReasonIfDisabled != decisionWhyBotAuthorBranch {
				t.Fatalf("direction option = %+v, want disabled", o)
			}
		case decisionActionApprove:
			if o.Enabled {
				t.Fatalf("approve option = %+v, want disabled", o)
			}
		}
	}

	caption, actor := stageTrackerCaption(stagelabels.NeedsDecision, stagelabels.LaneReasonBotAuthorFinding, "github-actions[bot]", "", false, stagelabels.Record{}, false, 0, "e9e24fd")
	if !strings.HasPrefix(caption, "Reviewer finding on another bot's PR") || actor != "maintainer" {
		t.Fatalf("caption = %q actor = %q", caption, actor)
	}
}

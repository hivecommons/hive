package dashboard

import (
	"strings"
	"testing"
)

// projectbluefin/server#686: GitHub holds the fork head's workflow runs and
// the hive's approval policy does not approve them. The 085 popover names the
// runs and the person's step; Approve stays enabled, since an approval of
// this head is what the approval sweep honours.
func TestDecisionForPRHeldWorkflowRuns(t *testing.T) {
	it := StageTrackerItem{Repo: "acme/server", Number: 686, Kind: "pr", Stage: trackerNumberNeedsDecision, Head: "e52fe17d6f4d", Reason: "workflow-approval-required:held-runs:44261912124,44257752419"}
	d := decisionForPR(it, nil, "", false)
	if d == nil || d.AskedBy != decisionAskedByLane ||
		!strings.Contains(d.Question, "workflow runs 44261912124, 44257752419 on e52fe17") ||
		!strings.Contains(d.Question, "Approve and run") {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Blocker, "held workflow runs are approved") || !strings.Contains(d.Blocker, "will not update the branch") {
		t.Fatalf("blocker = %q", d.Blocker)
	}
	for _, o := range d.Options {
		if o.Action == decisionActionApprove && !o.Enabled {
			t.Fatalf("approve option = %+v, want enabled", o)
		}
	}
	if got := decisionHeldRunsWords([]string{"7"}); got != "workflow run 7" {
		t.Fatalf("one run reads %q", got)
	}
}

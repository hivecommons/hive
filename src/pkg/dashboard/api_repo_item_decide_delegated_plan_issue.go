package dashboard

import (
	"strconv"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// delegatedPlanIssueWording rewords an approval on an issue
// (hivecommons/hive#15754). delegatedPlanWording speaks PR: the plan for
// issue #15734 read "Approve #15734 as it is and let the merge queue merge
// it" — but an issue has no head to approve and nothing to merge. Approving
// an issue records the direction and lifts its decision park, so an agent
// can pick it up. Any other decision keeps its wording.
func delegatedPlanIssueWording(in delegatedecision.Input, d delegatedecision.Decision, summary string, consequences []string) (string, []string) {
	if in.Kind != delegatedecision.KindIssue || d.Verdict != delegatedecision.VerdictApprove {
		return summary, consequences
	}
	ref := "#" + strconv.Itoa(in.Number)
	return "Approve the direction of " + ref + " and let the hive pick it up.",
		[]string{"Hive records the approval and lifts the decision park on " + ref + ".", "An agent claims " + ref + " and opens a PR for it."}
}

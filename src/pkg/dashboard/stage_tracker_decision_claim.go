package dashboard

import "strconv"

// decisionForClaimConflict: the merge queue holds this PR because an open
// PR in another lane claims the same issue and nobody has decided which
// lands (hivecommons/hive#15810). The options name the concrete PRs —
// "Keep #15793 (this PR)", "Close #15793 as superseded by #15783" — never
// a generic "all N PRs": Approve admits this one to the queue, Close ends
// the conflict in the other's favour, and the hive picks neither itself.
func decisionForClaimConflict(it StageTrackerItem, held bool) *StageTrackerDecision {
	this := "#" + strconv.Itoa(it.Number)
	other := "#" + strconv.Itoa(it.MergeGate.DuplicateOf)
	issue := "the same issue"
	if it.MergeGate.DuplicateIssue > 0 {
		issue = "issue #" + strconv.Itoa(it.MergeGate.DuplicateIssue)
	}
	d := &StageTrackerDecision{Since: it.StageSince, AskedBy: decisionAskedByLane, BlockerReason: it.Reason}
	d.Question = this + " and " + other + " both claim " + issue + ". Keep " + this + " and close " + other + ", or keep " + other + " and close " + this + "?"
	d.Blocker = "The merge queue admits neither PR while two open PRs claim one issue and no decision is recorded, so two implementations of one fix never both land."
	if held {
		d.Blocker += " " + decisionHeldBlocker(it.Hold)
	}
	keep := decisionOption(decisionActionApprove, "Keep "+this+" — admit it to the merge queue", !held, decisionWhyHeld)
	keep.Consequence = "Hive records your approval on " + this + " and the queue admits it on the next pass; close " + other + " yourself or from its own row so it does not land too."
	closeThis := decisionOption(decisionActionCloseSuperseded, "Close "+this+" as superseded by "+other, !held, decisionWhyHeld)
	closeThis.Consequence = this + " is closed with a comment crediting the work and pointing at " + other + "; " + other + " then proceeds alone."
	d.Options = []StageTrackerDecisionOption{keep, closeThis, decisionHoldOption(held)}
	return d
}

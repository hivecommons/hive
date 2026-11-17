package stagelabels

import (
	"strings"
	"testing"
)

// forkGuardFenceEvidence is projectbluefin/utah-packages#1181 at head
// 11352dc: a fork PR that edits workflows, whose required `Canary` check
// refuses fork PRs, with the reviewer swarm's requires_human verdict and the
// workflow-path fence awaiting the owner's approval.
func forkGuardFenceEvidence() Evidence {
	e := readyEvidence()
	e.Cause = CauseDecision
	e.Verdict = VerdictRequiresHuman
	e.DecisionQuestion = "Is the new release step safe to run?"
	e.ManualMerge = true
	e.ApprovalReleasesFence = true
	e.CauseReason = "the PR edits CI workflow / action files: awaiting owner approval"
	e.CauseAwaitsApproval = true
	e.ForkGuard = []string{"Canary"}
	e.Checks = CheckSummary{Total: 55, Green: 54, Red: []string{"Canary"}, Known: true}
	return e
}

// With a trusted approval on the head, the 085 note names only the fork
// guard and says nothing is left to approve. Before, it kept the
// requires_human verdict, the reviewer's question and "awaiting owner
// approval", and asked a trusted merger to approve the head again.
func TestRouteForkGuardOnApprovedHeadListsOnlyTheGuard(t *testing.T) {
	e := forkGuardFenceEvidence()
	e.HumanApprover = "Danathar"
	r := RouteHuman(e)
	if r.Stage != NeedsDecision || len(r.Reasons) != 1 || !strings.Contains(r.Reasons[0], "`Canary`") {
		t.Fatalf("route = %+v, want 085 on the fork guard alone", r)
	}
	if r.HumanApprover != "Danathar" {
		t.Fatalf("route approver = %q", r.HumanApprover)
	}
	body := DecisionComment(r)
	for _, gone := range []string{"the reviewer swarm returned `requires_human`", "reviewer question", "awaiting owner approval", "a trusted merger approves this head"} {
		if strings.Contains(body, gone) {
			t.Fatalf("085 note still says %q:\n%s", gone, body)
		}
	}
	if !strings.Contains(body, "nothing more to approve: @Danathar approved this head (`abc1234`)") {
		t.Fatalf("085 note does not say the approval is done:\n%s", body)
	}
}

// Without the approval the note is unchanged: fork guard, requires_human,
// the reviewer's question, the fence awaiting approval, and the ask.
func TestRouteForkGuardWithoutApprovalStillAsks(t *testing.T) {
	r := RouteHuman(forkGuardFenceEvidence())
	if r.Stage != NeedsDecision {
		t.Fatalf("stage = %v", r.Stage)
	}
	body := DecisionComment(r)
	for _, want := range []string{
		"`Canary`",
		"the reviewer swarm returned `requires_human` @abc1234",
		"reviewer question: Is the new release step safe to run?",
		"awaiting owner approval",
		"a trusted merger approves this head (`abc1234`)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("085 note missing %q:\n%s", want, body)
		}
	}
}

// A manual workflow fence (the owner-approval release off) is not released
// by the approval: the approved head is a person's merge, as before.
func TestRouteManualFenceApprovedStaysAPersonsMerge(t *testing.T) {
	e := readyEvidence()
	e.Cause = CauseDecision
	e.Verdict = VerdictRequiresHuman
	e.ManualMerge = true
	e.ManualMergeRule = true
	e.CauseReason = "the PR edits CI workflow / action files, which the lane never merges unattended"
	e.LaneReason = "contributor-lane:workflow-path"
	e.HumanApprover = "Danathar"
	r := RouteHuman(e)
	if r.Stage != ReadyForHumanMerge || !r.ManualMerge || len(r.Reasons) != 1 || !strings.Contains(r.Reasons[0], "the only step left is the merge") {
		t.Fatalf("route = %+v, want 090 for the person's merge", r)
	}
	// A cause flagged as awaiting approval on a manual rule is still listed.
	e.CauseAwaitsApproval = true
	if r := RouteHuman(e); r.Stage != ReadyForHumanMerge {
		t.Fatalf("manual rule with the flag set: route = %+v", r)
	}
}

// The released fence was the only reason: the head is not parked at 085 on
// "the automerge lane will not merge this unattended".
func TestRouteReleasedFenceAloneIsNotADecision(t *testing.T) {
	e := forkGuardFenceEvidence()
	e.ForkGuard = nil
	e.Checks = CheckSummary{Total: 55, Green: 55, Known: true}
	e.Verdict = VerdictApproved
	e.HumanApprover = "Danathar"
	r := RouteHuman(e)
	if r.Stage == NeedsDecision {
		t.Fatalf("route = %+v, want no decision for a released fence", r)
	}
	for _, reason := range r.Reasons {
		if strings.Contains(reason, decisionConsequenceReason) || strings.Contains(reason, "awaiting owner approval") {
			t.Fatalf("route reasons = %q", r.Reasons)
		}
	}
}

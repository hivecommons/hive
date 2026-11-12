package stagelabels

import (
	"strings"
	"testing"
)

// The 085 "What to do" texts the tests below pin. The approval asks and the
// sentinel-only step are the strings DecisionActions already rendered; the
// two combined steps are new.
const (
	testAskSelfAuthored = "approve this head (`abc1234`) to merge as-is — a GitHub **Approve** review, or a `/hive approve` reply posted after the verdict — or leave a *changes requested* review (or reply `/hive decision <what to change>`) and the hive will push the fix"
	testAskOtherAuthor  = "a trusted merger approves this head (`abc1234`) — a GitHub **Approve** review, or a `/hive approve` reply posted after the verdict — and the hive re-evaluates on its next pass; the PR author cannot release their own PR, and the hive does not push to a PR it did not open"
	testAskFence        = "a trusted merger approves this head (`abc1234`) with a GitHub **Approve** review (or a `/hive decision approve` reply, typed here or from the dashboard), which releases the CI workflow-file fence, and the hive merges the PR once the other gates clear; approval on an earlier head carries over only the hive's own merge of the base branch (diff unchanged), and a `/hive approve` reply does not release the fence"
	testAskFenceVerdict = "a trusted merger approves this head (`abc1234`) with a GitHub **Approve** review (or a `/hive decision approve` reply, typed here or from the dashboard), which releases the CI workflow-file fence and settles the reviewer's `requires_human` verdict, and the hive merges the PR once the other gates clear; approval on an earlier head carries over only the hive's own merge of the base branch (diff unchanged), and a `/hive approve` reply does not release the fence"
	testAlreadyApproved = "nothing more to approve: @alice approved this head (`abc1234`); resolve the other reasons listed above"

	testSentinelOnly = "resolve the sentinel alert and have a maintainer clear its label"
	// One dashboard Approve answers both; the review and the typed reply
	// approve only.
	testSentinelOneApprove      = "one action answers both the approval above and the sentinel alert: **Approve** on this PR's row in the hive dashboard's Activity monitor (or `approve` through the admin MCP's `repository.item_decision`) records the approval and clears the alert's label, so no separate label removal is needed; a GitHub **Approve** review or a `/hive approve` reply approves but leaves the label for a maintainer to remove"
	testSentinelOneApproveFence = "one action answers both the approval above and the sentinel alert: **Approve** on this PR's row in the hive dashboard's Activity monitor (or `approve` through the admin MCP's `repository.item_decision`) records the approval and clears the alert's label, so no separate label removal is needed; a GitHub **Approve** review or a typed `/hive decision approve` reply approves but leaves the label for a maintainer to remove"
	// The approval is on record: only the label is left for the alert.
	testSentinelLabelLeft = "the sentinel alert is still open and needs no second approval: the only step left for it is clearing its label — a maintainer removes the label on GitHub, or presses **Confirm safe** on this PR's row in the hive dashboard's Activity monitor"
)

// testWhatToDo renders steps the way DecisionActions does: one step inline,
// several as a list.
func testWhatToDo(steps ...string) string {
	if len(steps) == 1 {
		return "**What to do:** " + steps[0] + "."
	}
	return "**What to do:**\n\n- " + strings.Join(steps, ".\n- ") + "."
}

// A PR parked on a person's approval that also carries a sentinel alert was
// told two things: "a trusted merger approves this head" and "resolve the
// sentinel alert and have a maintainer clear its label". A maintainer who
// removed the label by hand found the PR still at 085 and had to approve as
// well, although Approve from the hive dashboard does both. The combined
// step now names that one action and what the alternatives leave undone.
func TestDecisionActionsNameTheOneApproveThatClearsSentinel(t *testing.T) {
	const head = "abc1234567890"
	for _, tc := range []struct {
		name  string
		route Route
		want  string
	}{
		{
			name:  "requires_human on another author's PR",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, Sentinel: true},
			want:  testWhatToDo(testAskOtherAuthor, testSentinelOneApprove),
		},
		{
			name:  "requires_human on a hive-authored PR",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, SelfAuthored: true, Sentinel: true},
			want:  testWhatToDo(testAskSelfAuthored, testSentinelOneApprove),
		},
		{
			name:  "releasable workflow fence",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, ManualMerge: true, ApprovalReleasesFence: true, Sentinel: true},
			want:  testWhatToDo(testAskFence, testSentinelOneApproveFence),
		},
		{
			name:  "releasable workflow fence with requires_human",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, ManualMerge: true, ApprovalReleasesFence: true, Sentinel: true},
			want:  testWhatToDo(testAskFenceVerdict, testSentinelOneApproveFence),
		},
		{
			name:  "requires_human already approved",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, HumanApprover: "@alice", Sentinel: true},
			want:  testWhatToDo(testAlreadyApproved, testSentinelLabelLeft),
		},
		{
			name:  "releasable workflow fence already approved",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, ManualMerge: true, ApprovalReleasesFence: true, HumanApprover: "alice", Sentinel: true},
			want:  testWhatToDo(testAlreadyApproved, testSentinelLabelLeft),
		},
		{
			name:  "a hold label keeps its own step after the combined one",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, Sentinel: true, Holds: []string{"do-not-merge/wip"}},
			want:  testWhatToDo(testAskOtherAuthor, testSentinelOneApprove, "resolve the reason for `do-not-merge/wip` and have a maintainer remove that label (`/hive approve` does not remove it)"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecisionActions(tc.route)
			if got != tc.want {
				t.Fatalf("DecisionActions =\n%s\nwant\n%s", got, tc.want)
			}
			if strings.Contains(got, testSentinelOnly) {
				t.Fatalf("combined case still lists the label removal as a separate step:\n%s", got)
			}
		})
	}

	// The same texts through RouteHuman and the whole 085 comment, so the
	// evidence's sentinel flag is shown to reach the step.
	e := readyEvidence()
	e.Verdict = VerdictRequiresHuman
	e.Sentinel = true
	r := RouteHuman(e)
	if r.Stage != NeedsDecision || !r.Sentinel {
		t.Fatalf("route = %+v", r)
	}
	if got := DecisionComment(r); !strings.Contains(got, testWhatToDo(testAskOtherAuthor, testSentinelOneApprove)) {
		t.Fatalf("085 comment does not name the one action:\n%s", got)
	}
	e.SelfAuthored = true
	if got := DecisionComment(RouteHuman(e)); !strings.Contains(got, testWhatToDo(testAskSelfAuthored, testSentinelOneApprove)) {
		t.Fatalf("hive-authored 085 comment does not name the one action:\n%s", got)
	}
}

// Routes with only one of the two — a sentinel alert and no approval owed,
// or an approval owed and no sentinel alert — keep their text exactly.
func TestDecisionActionsSentinelOnlyAndApprovalOnlyUnchanged(t *testing.T) {
	const head = "abc1234567890"
	for _, tc := range []struct {
		name  string
		route Route
		want  string
	}{
		{
			name:  "sentinel alert, verdict approved",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, Sentinel: true},
			want:  testWhatToDo(testSentinelOnly),
		},
		{
			name:  "sentinel alert, no reviewer configured",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictUnavailable, Sentinel: true},
			want:  testWhatToDo(testSentinelOnly),
		},
		{
			name:  "sentinel alert, approver recorded but no approval was owed",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, HumanApprover: "alice", Sentinel: true},
			want:  testWhatToDo(testSentinelOnly),
		},
		{
			name:  "sentinel alert and a hold label",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, Sentinel: true, Holds: []string{"do-not-merge/wip"}},
			want:  testWhatToDo(testSentinelOnly, "resolve the reason for `do-not-merge/wip` and have a maintainer remove that label (`/hive approve` does not remove it)"),
		},
		{
			name:  "requires_human on another author's PR",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman},
			want:  testWhatToDo(testAskOtherAuthor),
		},
		{
			name:  "requires_human on a hive-authored PR",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, SelfAuthored: true},
			want:  testWhatToDo(testAskSelfAuthored),
		},
		{
			name:  "releasable workflow fence",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictApproved, ManualMerge: true, ApprovalReleasesFence: true},
			want:  testWhatToDo(testAskFence),
		},
		{
			name:  "releasable workflow fence with requires_human",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, ManualMerge: true, ApprovalReleasesFence: true},
			want:  testWhatToDo(testAskFenceVerdict),
		},
		{
			name:  "approval already recorded",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, HumanApprover: "alice"},
			want:  testWhatToDo(testAlreadyApproved),
		},
		{
			name:  "manual-merge rule no approval releases, sentinel alert open",
			route: Route{Stage: NeedsDecision, Head: head, Verdict: VerdictRequiresHuman, ManualMerge: true, Sentinel: true},
			want:  "**What to do:** a maintainer must review and merge this PR manually; an approving review or `/hive approve` does not override this lane's manual-merge rule.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecisionActions(tc.route); got != tc.want {
				t.Fatalf("DecisionActions =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

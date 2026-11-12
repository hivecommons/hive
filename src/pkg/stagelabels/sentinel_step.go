package stagelabels

import "strings"

// sentinelLabelOnlyStep is the 085 step for an open sentinel alert on a
// route that asks for no approval: the alert is all the step is about.
const sentinelLabelOnlyStep = "resolve the sentinel alert and have a maintainer clear its label"

// oneApproveSentinelStep is the "What to do" step DecisionActions lists for
// an open sentinel alert (Route.Sentinel). It mirrors the approval branches
// of DecisionActions, which has already returned for a manual-merge rule no
// approval releases:
//
//   - No approval is asked (neither `requires_human` nor a releasable
//     fence): sentinelLabelOnlyStep, as before.
//   - An approval is asked: the 085 comment used to list "a trusted merger
//     approves this head" and "have a maintainer clear its label" as two
//     steps, and a maintainer who removed the label by hand then had to
//     approve as well. One action does both — the dashboard's Approve on
//     the PR (the Activity monitor row, or `approve` through the admin
//     MCP's `repository.item_decision`, the same endpoint) records the
//     approval for the head and takes the alert's label off
//     (github.Client.RecordOperatorPullRequestApprovalWith with
//     SentinelLabel). A GitHub Approve review and a typed `/hive approve` or
//     `/hive decision approve` reply approve the head and remove no label.
//   - The approval is already on record (Route.HumanApprover): only the
//     label is left for the alert, removed on GitHub or with the row's
//     Confirm safe, which clears the label and records no approval.
func oneApproveSentinelStep(r Route) string {
	if (r.ManualMerge && !r.ApprovalReleasesFence) || (!r.ManualMerge && r.Verdict != VerdictRequiresHuman) {
		return sentinelLabelOnlyStep
	}
	if strings.TrimPrefix(strings.TrimSpace(r.HumanApprover), "@") != "" {
		return "the sentinel alert is still open and needs no second approval: the only step left for it is clearing its label — a maintainer removes the label on GitHub, or presses **Confirm safe** on this PR's row in the hive dashboard's Activity monitor"
	}
	// The fence step names `/hive decision approve` as its typed approval
	// (`/hive approve` does not release the fence); the others name
	// `/hive approve`.
	reply := "a `/hive approve` reply"
	if r.ManualMerge {
		reply = "a typed `/hive decision approve` reply"
	}
	return "one action answers both the approval above and the sentinel alert: **Approve** on this PR's row in the hive dashboard's Activity monitor (or `approve` through the admin MCP's `repository.item_decision`) records the approval and clears the alert's label, so no separate label removal is needed; a GitHub **Approve** review or " + reply + " approves but leaves the label for a maintainer to remove"
}

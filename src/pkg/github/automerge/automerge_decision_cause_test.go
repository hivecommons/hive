package automerge

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// The stage goldens render the same prose from prstage; a reworded cause here
// that skips prstage would drift from them (hivecommons/hive#15384).
func TestDecisionCauseUsesSharedProse(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{contributorLaneReasonTestsRemoved, prstage.CauseTestsRemoved},
		{contributorLaneReasonReporterTrustHold, prstage.CauseReporterTrustHold},
		{autoMergeReasonHumanMergePath, prstage.CauseHumanMergePath},
		{autoMergeReasonIntentUnauthorized, prstage.CauseIntentUnauthorized},
		{"approval-desk-withheld", prstage.CauseApprovalDeskWithheld},
		{contributorLaneReasonWorkflowPath, prstage.CauseWorkflowPathManual},
		{workflowApprovalUntrustedPrefix, prstage.CauseWorkflowApprovalUntrusted()},
		{workflowApprovalWorkflowMissingPrefix, prstage.CauseWorkflowApprovalWorkflowMissing()},
		{workflowApprovalRequiredPrefix, prstage.CauseWorkflowApprovalRequired()},
		{"something-new", prstage.CauseLaneReported("something-new")},
	} {
		if got := decisionCause(tc.reason); got != tc.want {
			t.Errorf("decisionCause(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
	if got := decisionCauseFor(contributorLaneReasonWorkflowPath, false); got != prstage.CauseWorkflowPathAwaitingApproval {
		t.Errorf("decisionCauseFor(workflow-path, false) = %q, want the awaiting-approval cause", got)
	}
}

package prstage

import "testing"

// TestHumanMergeReason pins the 090 reason code for every lane reason that
// can land a PR at ready-for-human-merge (hivecommons/hive#14658): the
// workflow-path fence with and without the approver suffix, the trust
// admission reasons, GitHub refusals, the App's missing permission, and
// the policy fallback.
func TestHumanMergeReason(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"   ":                             "",
		ContributorLaneReasonWorkflowPath: HumanMergeReasonWorkflowFiles,
		"contributor-lane:workflow-path:human-approved@clubanderson":        HumanMergeReasonWorkflowFiles,
		"Contributor-Lane:Workflow-Path":                                    HumanMergeReasonWorkflowFiles,
		ContributorLaneReasonTestsRemoved:                                   HumanMergeReasonTestsRemoved,
		AutoMergeReasonHumanMergePath:                                       HumanMergeReasonManualMergePolicy,
		AutoMergeReasonHumanMergePathEvidence:                               HumanMergeReasonManualMergePolicy,
		AutoMergeReasonMergeRefusedReview:                                   HumanMergeReasonBranchProtection,
		AutoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting:         HumanMergeReasonBranchProtection,
		AutoMergeReasonMergeRefusedMethod:                                   HumanMergeReasonMergeMethod,
		AutoMergeReasonNativeQueueMethod:                                    HumanMergeReasonMergeMethod,
		"repo-auto-merge-disabled":                                          HumanMergeReasonAutoMergeOff,
		"untrusted-author-role":                                             HumanMergeReasonUntrustedAuthor,
		ContributorLaneReasonClaimTier:                                      HumanMergeReasonUntrustedAuthor,
		ContributorLaneReasonNoClaim:                                        HumanMergeReasonUntrustedAuthor,
		ContributorLaneReasonAuthorAssociation:                              HumanMergeReasonUntrustedAuthor,
		StageReasonUnauthorizedPrefix + UnauthorizedStepApproveWorkflowRuns: HumanMergeReasonUnauthorized,
		AutoMergeReasonIntentUnauthorized:                                   HumanMergeReasonUnauthorized,
		"reviewer-verdict:requires_human:human-approved@octocat":            HumanMergeReasonReviewerDeferred,
		"approval-desk-withheld":                                            HumanMergeReasonPolicy,
		"something-new":                                                     HumanMergeReasonPolicy,
	}
	for in, want := range cases {
		if got := HumanMergeReason(in); got != want {
			t.Errorf("HumanMergeReason(%q) = %q, want %q", in, got, want)
		}
	}
}

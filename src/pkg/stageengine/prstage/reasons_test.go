package prstage

import "testing"

func TestParseLinkedIssueReason(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		wantKind  string
		wantIssue int
		wantLabel string
		wantOK    bool
	}{
		{reason: LinkedIssueReason(LinkedIssueReasonHoldPrefix, 551, "hold"), wantKind: LinkedIssueReasonHoldPrefix, wantIssue: 551, wantLabel: "hold", wantOK: true},
		{reason: "linked-issue:needs-decision:#12:stage/045-needs-decision", wantKind: LinkedIssueReasonNeedsDecisionPrefix, wantIssue: 12, wantLabel: "stage/045-needs-decision", wantOK: true},
		{reason: "linked-issue:hold:#7:status: on hold", wantKind: LinkedIssueReasonHoldPrefix, wantIssue: 7, wantLabel: "status: on hold", wantOK: true},
		{reason: LinkedIssueReasonCheck},
		{reason: "linked-issue:hold:551:hold"},
		{reason: "linked-issue:hold:#x:hold"},
		{reason: "linked-issue:other:#5:hold"},
		{reason: "label:hold"},
	} {
		kind, issue, label, ok := ParseLinkedIssueReason(tc.reason)
		if kind != tc.wantKind || issue != tc.wantIssue || label != tc.wantLabel || ok != tc.wantOK {
			t.Fatalf("ParseLinkedIssueReason(%q) = %q, %d, %q, %v; want %q, %d, %q, %v", tc.reason, kind, issue, label, ok, tc.wantKind, tc.wantIssue, tc.wantLabel, tc.wantOK)
		}
	}
}

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

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

// TestClosingLinkReasons pins the closing-link reason shapes
// (hivecommons/hive#15025): the formatters round-trip through
// ParseClosingLinkReason, and anything without a `prefix#N` body with a
// positive N is not ok.
func TestClosingLinkReasons(t *testing.T) {
	if got := ClosingContradictionReason(1023); got != "closing-contradiction:#1023" {
		t.Fatalf("ClosingContradictionReason(1023) = %q", got)
	}
	if got := ClosingKeywordTargetsPRReason(1036); got != "closing-keyword-targets-pr:#1036" {
		t.Fatalf("ClosingKeywordTargetsPRReason(1036) = %q", got)
	}
	for _, tc := range []struct {
		reason     string
		wantPrefix string
		wantNumber int
		wantOK     bool
	}{
		{reason: ClosingContradictionReason(1023), wantPrefix: ClosingContradictionReasonPrefix, wantNumber: 1023, wantOK: true},
		{reason: ClosingKeywordTargetsPRReason(1036), wantPrefix: ClosingKeywordTargetsPRReasonPrefix, wantNumber: 1036, wantOK: true},
		{reason: "closing-contradiction:#1", wantPrefix: ClosingContradictionReasonPrefix, wantNumber: 1, wantOK: true},
		{reason: "closing-contradiction:1023"},
		{reason: "closing-contradiction:#"},
		{reason: "closing-contradiction:#x"},
		{reason: "closing-contradiction:#0"},
		{reason: "closing-keyword-targets-pr:#-4"},
		{reason: "closing-keyword-targets-pr:#12:extra"},
		{reason: LinkedIssueReason(LinkedIssueReasonHoldPrefix, 551, "hold")},
		{reason: ""},
	} {
		prefix, number, ok := ParseClosingLinkReason(tc.reason)
		if prefix != tc.wantPrefix || number != tc.wantNumber || ok != tc.wantOK {
			t.Fatalf("ParseClosingLinkReason(%q) = %q, %d, %v; want %q, %d, %v", tc.reason, prefix, number, ok, tc.wantPrefix, tc.wantNumber, tc.wantOK)
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

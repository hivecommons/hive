package prstage

import (
	"strconv"
	"strings"
)

// Lane reasons are shared by the producers and the stage classifier.
const (
	ReviewGateOutcomeAwaiting              = "awaiting-review-approval"
	ReviewGateOutcomeChangesRequested      = "changes-requested"
	ReviewGateOutcomeNoBundle              = "no-review-bundle"
	AutoMergeReasonBehindBudgetExhausted   = "behind-update-budget-exhausted"
	AutoMergeReasonForkNotEditable         = "fork-not-editable"
	AutoMergeReasonHumanChangesRequested   = "human-changes-requested"
	AutoMergeReasonHumanMergePath          = "human-merge-path"
	AutoMergeReasonHumanMergePathEvidence  = "human-merge-path-evidence"
	AutoMergeReasonIntentEvidence          = "intent-evidence"
	AutoMergeReasonIntentUnauthorized      = "intent-unauthorized"
	AutoMergeReasonMergeQueueRequired      = "merge-queue-required"
	AutoMergeReasonMergeRefusedMethod      = "merge-refused:method-not-allowed"
	AutoMergeReasonMergeRefusedReview      = "merge-refused:review-required"
	AutoMergeReasonNoAppBotLogin           = "no-app-bot-login"
	AutoMergeReasonReviewGatePrefix        = "review-gate:"
	AutoMergeReasonWorkflowPermission      = "workflow-permission"
	ContributorLaneReasonAdoptedByHive     = "contributor-lane:adopted-by-hive"
	ContributorLaneReasonAppAuthored       = "contributor-lane:app-authored"
	ContributorLaneReasonAuthorAssociation = "contributor-lane:author-association"
	ContributorLaneReasonChangesRequested  = "contributor-lane:changes-requested"
	ContributorLaneReasonClaimAuthor       = "contributor-lane:claim-author-mismatch"
	ContributorLaneReasonClaimOtherHive    = "contributor-lane:claim-other-hive"
	ContributorLaneReasonClaimTier         = "contributor-lane:claim-tier-untrusted"
	ContributorLaneReasonClaimUnverified   = "contributor-lane:claim-unverified"
	ContributorLaneReasonFilesEvidence     = "contributor-lane:files-evidence"
	ContributorLaneReasonNoAppBotLogin     = "contributor-lane:no-app-bot-login"
	ContributorLaneReasonNoAuthor          = "contributor-lane:no-author"
	ContributorLaneReasonNoClaim           = "contributor-lane:no-claim"
	ContributorLaneReasonNoLinkedIssue     = "contributor-lane:no-linked-issue"
	ContributorLaneReasonNoReviewer        = "contributor-lane:no-reviewer"
	ContributorLaneReasonNoVerdict         = "contributor-lane:no-verdict"
	ContributorLaneReasonReporterTrustHold = "contributor-lane:reporter-trust-hold"
	ContributorLaneReasonSentinelBlocked   = "contributor-lane:sentinel-blocked"
	ContributorLaneReasonSoaking           = "contributor-lane:soaking"
	ContributorLaneReasonTestsRemoved      = "contributor-lane:tests-removed"
	ContributorLaneReasonWorkflowPath      = "contributor-lane:workflow-path"
	LinkedIssueReasonCheck                 = "linked-issue-check"
	LinkedIssueReasonHoldPrefix            = "linked-issue:hold:"
	LinkedIssueReasonNeedsDecisionPrefix   = "linked-issue:needs-decision:"
	StageLaneContributor                   = "contributor-lane"
	StageLaneSelf                          = "self-authored"
	StageLaneTrusted                       = "trusted-author"
	StageReasonBaseBranchRed               = "base-branch-red:"
	StageReasonForkGuard                   = "fork-guard:"
	StageReasonOptionalCheckFailing        = "optional-check-failing:"
	StageReasonSharedCIIncident            = "shared-ci-incident:"
	TrustedAuthorReasonAppAuthored         = "trusted-author:app-authored"
	TrustedBotLaneSkipReason               = "trusted-bot-lane"
	WorkflowApprovalRequiredPrefix         = "workflow-approval-required:"
)

// LinkedIssueReason formats the refusal for an open issue the PR closes that
// carries label: kindPrefix is LinkedIssueReasonHoldPrefix or
// LinkedIssueReasonNeedsDecisionPrefix, so the result reads
// `linked-issue:hold:#551:hold`.
func LinkedIssueReason(kindPrefix string, issue int, label string) string {
	return kindPrefix + "#" + strconv.Itoa(issue) + ":" + label
}

// ParseLinkedIssueReason splits a LinkedIssueReason back into its kind
// prefix, issue number and label. ok is false for any other reason,
// including the fail-closed LinkedIssueReasonCheck.
func ParseLinkedIssueReason(reason string) (kindPrefix string, issue int, label string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(reason, LinkedIssueReasonHoldPrefix):
		kindPrefix, rest = LinkedIssueReasonHoldPrefix, strings.TrimPrefix(reason, LinkedIssueReasonHoldPrefix)
	case strings.HasPrefix(reason, LinkedIssueReasonNeedsDecisionPrefix):
		kindPrefix, rest = LinkedIssueReasonNeedsDecisionPrefix, strings.TrimPrefix(reason, LinkedIssueReasonNeedsDecisionPrefix)
	default:
		return "", 0, "", false
	}
	rest, found := strings.CutPrefix(rest, "#")
	if !found {
		return "", 0, "", false
	}
	num, label, _ := strings.Cut(rest, ":")
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, "", false
	}
	return kindPrefix, n, label, true
}

package prstage

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
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
	// StageReasonUnauthorizedPrefix: the lane's next step needs an API
	// call the hive's GitHub App is not permitted to make (403 "Resource
	// not accessible by integration"); the rest names the step
	// (UnauthorizedStepApproveWorkflowRuns, …). A person must take the
	// step; the lane parks at 085 (hivecommons/hive#14589).
	StageReasonUnauthorizedPrefix = "unauthorized:"
	// UnauthorizedStepApproveWorkflowRuns: approving held `action_required`
	// workflow runs needs `actions: write`.
	UnauthorizedStepApproveWorkflowRuns = "approve-workflow-runs"
	// UnauthorizedStepMergeWorkflowPath: merging an owner-approved PR that
	// edits `.github/workflows/**` needs `workflows: write`; without it the
	// lane parks the PR for a person's merge click (hivecommons/hive#14664).
	UnauthorizedStepMergeWorkflowPath = "merge-workflow-path"
	TrustedAuthorReasonAppAuthored    = "trusted-author:app-authored"
	TrustedBotLaneSkipReason          = "trusted-bot-lane"
	WorkflowApprovalRequiredPrefix    = "workflow-approval-required:"
)

// Closing-link reasons (hivecommons/hive#15025): a closing keyword the
// merge would act on names the wrong thing. The rest of each is `#N`.
const (
	// ClosingContradictionReasonPrefix: the PR body closes #N and also says
	// #N stays open or is partial, so GitHub would close #N on merge
	// (hivecommons/hive#15025). The rest is `#N`; a body edit clears it.
	ClosingContradictionReasonPrefix = "closing-contradiction:"
	// ClosingKeywordTargetsPRReasonPrefix: a closing keyword in the PR body
	// or a commit message on the head names another pull request in the
	// repository, which GitHub would close on merge (hivecommons/hive#15025).
	// The rest is `#N`.
	ClosingKeywordTargetsPRReasonPrefix = "closing-keyword-targets-pr:"
)

// HumanMergeReason codes: why a PR parked at 090 ready-for-human-merge
// needs a person's merge click rather than the hive's. Derived from the
// lane reason the stage record already carries, so the dashboard, the
// comments and the logs name the same cause (hivecommons/hive#14658).
const (
	// HumanMergeReasonWorkflowFiles: the PR edits .github/workflows/** or
	// action files; the hive never merges those (and its App lacks the
	// `workflows` permission).
	HumanMergeReasonWorkflowFiles = "workflow-files"
	// HumanMergeReasonTestsRemoved: the PR deletes tests.
	HumanMergeReasonTestsRemoved = "tests-removed"
	// HumanMergeReasonManualMergePolicy: the PR touches a configured
	// human-merge path (auto_merge.human_merge_paths).
	HumanMergeReasonManualMergePolicy = "manual-merge-policy"
	// HumanMergeReasonBranchProtection: GitHub's branch protection or
	// rulesets want an approving review the App cannot give.
	HumanMergeReasonBranchProtection = "branch-protection"
	// HumanMergeReasonMergeMethod: the repository disallows the merge
	// method the hive must use for this PR.
	HumanMergeReasonMergeMethod = "merge-method"
	// HumanMergeReasonAutoMergeOff: auto-merge is switched off for the
	// repository or the lane.
	HumanMergeReasonAutoMergeOff = "auto-merge-off"
	// HumanMergeReasonUntrustedAuthor: no lane can vouch for the author
	// (tier below require_tiers, no verified claim, no trusted role).
	HumanMergeReasonUntrustedAuthor = "untrusted-author"
	// HumanMergeReasonUnauthorized: the hive's GitHub App lacks the
	// permission for the merge or its intent could not be authorized.
	HumanMergeReasonUnauthorized = "unauthorized"
	// HumanMergeReasonReviewerDeferred: the reviewer swarm returned
	// requires_human and a trusted merger approved the head; the hive
	// leaves the merge itself to a person.
	HumanMergeReasonReviewerDeferred = "reviewer-deferred"
	// HumanMergeReasonPolicy: a lane rule no more specific code names.
	HumanMergeReasonPolicy = "policy"
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

// ClosingContradictionReason formats the refusal for a PR body that closes
// issue and says it stays open: `closing-contradiction:#1023`.
func ClosingContradictionReason(issue int) string {
	return ClosingContradictionReasonPrefix + "#" + strconv.Itoa(issue)
}

// ClosingKeywordTargetsPRReason formats the refusal for a closing keyword
// that names pull request number: `closing-keyword-targets-pr:#1036`.
func ClosingKeywordTargetsPRReason(number int) string {
	return ClosingKeywordTargetsPRReasonPrefix + "#" + strconv.Itoa(number)
}

// ParseClosingLinkReason splits a ClosingContradictionReason or a
// ClosingKeywordTargetsPRReason into its prefix and number. ok is false for
// any other reason.
func ParseClosingLinkReason(reason string) (prefix string, number int, ok bool) {
	for _, p := range []string{ClosingContradictionReasonPrefix, ClosingKeywordTargetsPRReasonPrefix} {
		rest, found := strings.CutPrefix(reason, p+"#")
		if !found {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			return "", 0, false
		}
		return p, n, true
	}
	return "", 0, false
}

// humanMergeReasonTrust are the admission reasons that mean no lane can
// vouch for the author (see stageengine.stageAdmissionReasons).
var humanMergeReasonTrust = map[string]bool{
	"untrusted-author-role": true, "author-permission-missing": true, "fork-non-member": true, "untrusted-merger": true,
	ContributorLaneReasonNoClaim: true, ContributorLaneReasonClaimAuthor: true, ContributorLaneReasonClaimOtherHive: true,
	ContributorLaneReasonNoLinkedIssue: true, ContributorLaneReasonClaimTier: true, ContributorLaneReasonClaimUnverified: true,
	ContributorLaneReasonAuthorAssociation: true,
}

// HumanMergeReason maps a 090 stage reason (with or without the
// `:human-approved@login` suffix) to its HumanMergeReason* code. An empty
// reason yields "" — the caller has nothing to explain; anything else is at
// least HumanMergeReasonPolicy.
func HumanMergeReason(reason string) string {
	base, _ := stagelabels.HumanApprovedReason(reason)
	base = strings.ToLower(strings.TrimSpace(base))
	switch {
	case base == "":
		return ""
	case base == ContributorLaneReasonWorkflowPath:
		return HumanMergeReasonWorkflowFiles
	case base == ContributorLaneReasonTestsRemoved:
		return HumanMergeReasonTestsRemoved
	case base == AutoMergeReasonHumanMergePath, base == AutoMergeReasonHumanMergePathEvidence:
		return HumanMergeReasonManualMergePolicy
	case base == AutoMergeReasonMergeRefusedReview, strings.HasPrefix(base, AutoMergeReasonReviewGatePrefix):
		return HumanMergeReasonBranchProtection
	case base == AutoMergeReasonMergeRefusedMethod:
		return HumanMergeReasonMergeMethod
	case base == "repo-auto-merge-disabled", base == StageLaneContributor+":disabled", base == "disabled":
		return HumanMergeReasonAutoMergeOff
	case humanMergeReasonTrust[base]:
		return HumanMergeReasonUntrustedAuthor
	case strings.HasPrefix(base, StageReasonUnauthorizedPrefix), base == AutoMergeReasonIntentUnauthorized,
		base == AutoMergeReasonIntentEvidence, base == "no-merger-authorizer":
		return HumanMergeReasonUnauthorized
	case strings.HasPrefix(base, "reviewer-verdict:requires_human"):
		return HumanMergeReasonReviewerDeferred
	}
	return HumanMergeReasonPolicy
}

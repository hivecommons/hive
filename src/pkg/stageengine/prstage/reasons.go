package prstage

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// SupersessionPendingLabel records the sweep's unresolved supersession park.
// It is independent of CI/head state and is removed when the cause clears.
const SupersessionPendingLabel = "hive/supersession-pending"
const ReasonSupersessionPending = "supersession-decision-pending"

// StageLaneWorkflow records the open PRs of a repository bound to a
// rules-mode workflow model that no auto-merge lane listed this pass
// (auto-merge off, below ACMM L6): the model's rules derive their stage, so
// the stage pass must see them (hivecommons/hive#19768). Its reason,
// StageReasonWorkflowModel, is a hand-off: the lane has no built-in opinion.
const (
	StageLaneWorkflow        = "workflow-model"
	StageReasonWorkflowModel = "workflow-model"
)

// Lane reasons are shared by the producers and the stage classifier.
const (
	ReviewGateOutcomeAwaiting             = "awaiting-review-approval"
	ReviewGateOutcomeChangesRequested     = "changes-requested"
	ReviewGateOutcomeNoBundle             = "no-review-bundle"
	AutoMergeReasonBehindBudgetExhausted  = "behind-update-budget-exhausted"
	AutoMergeReasonForkNotEditable        = "fork-not-editable"
	AutoMergeReasonForkWorkflowUpdate     = "fork-workflow-update"
	AutoMergeReasonHumanChangesRequested  = stagelabels.LaneReasonHumanChangesRequested
	AutoMergeReasonHumanMergePath         = "human-merge-path"
	AutoMergeReasonHumanMergePathEvidence = "human-merge-path-evidence"
	// AutoMergeReasonSensitivePathDecision: the PR touches an
	// auto_merge.decision_gate path (credential, keystore, auth, secret)
	// and no person has decided on this head; no lane merges it until one
	// does, whoever the author is.
	AutoMergeReasonSensitivePathDecision = "sensitive-path-decision"
	// AutoMergeReasonSensitivePathEvidence: the decision gate could not read
	// the complete changed-file list. Fails closed.
	AutoMergeReasonSensitivePathEvidence = "sensitive-path-evidence"
	// AutoMergeReasonSentinelDecision: the PR wears the sentinel alert label
	// and no person has decided on this head (auto_merge.decision_gate.sentinel).
	AutoMergeReasonSentinelDecision = "sentinel-decision"
	// AutoMergeReasonHumanMergeRequired: the PR's title says breaking
	// change (⚠️, or an auto_merge.human_merge_title_prefixes prefix) or it
	// wears an auto_merge.human_merge_labels label; no lane and no merge
	// train ever merges it — the owner does (hivecommons/hive#15442).
	AutoMergeReasonHumanMergeRequired      = "human-merge-required"
	AutoMergeReasonIntentEvidence          = "intent-evidence"
	AutoMergeReasonIntentUnauthorized      = "intent-unauthorized"
	AutoMergeReasonMergeQueueRequired      = "merge-queue-required"
	AutoMergeReasonMergeRefusedMethod      = "merge-refused:method-not-allowed"
	AutoMergeReasonMergeRefusedReview      = "merge-refused:review-required"
	AutoMergeReasonNativeQueuePrefix       = "native-merge-queue"
	AutoMergeReasonNativeQueueMethod       = "native-merge-queue-method"
	AutoMergeReasonNativeQueued            = "native-merge-queued"
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
	// ContributorLaneReasonOperatorApproval is the admit reason when a
	// person's Approve admitted the head (hivecommons/hive#15136).
	ContributorLaneReasonOperatorApproval  = "contributor-lane:operator-approval"
	ContributorLaneReasonNoAuthor          = "contributor-lane:no-author"
	ContributorLaneReasonNoClaim           = "contributor-lane:no-claim"
	ContributorLaneReasonNoLinkedIssue     = "contributor-lane:no-linked-issue"
	ContributorLaneReasonNoReviewer        = "contributor-lane:no-reviewer"
	ContributorLaneReasonNoVerdict         = "contributor-lane:no-verdict"
	ContributorLaneReasonReporterTrustHold = "contributor-lane:reporter-trust-hold"
	ContributorLaneReasonReviewOff         = "contributor-lane:review-off"
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
	// StageReasonBaseRefreshMergeGuard: the lane did not evaluate the PR
	// this pass because a merge to its destination was in flight
	// (hivecommons/hive#16520); it says nothing about the stage.
	StageReasonBaseRefreshMergeGuard = "base-refresh:merge-guard"
	// StageReasonMergeableUnknown: GitHub reported mergeable_state
	// "unknown" (it is still computing the merge, as it does after the
	// base moves), so the contributor lane stopped before it evaluated the
	// PR; like the merge guard it says nothing about the stage.
	StageReasonMergeableUnknown     = "mergeable-unknown"
	StageReasonForkGuard            = stagelabels.LaneReasonForkGuardPrefix
	StageReasonOptionalCheckFailing = "optional-check-failing:"
	StageReasonSharedCIIncident     = "shared-ci-incident:"
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
	// UnauthorizedStepPostPRGateStatus: the hive posts the required PR-contents
	// check itself (auto_merge.pr_gate, enforce) and its installation lacks
	// Commit statuses write, or GitHub refused the write; an organization
	// owner accepts the permission (hivecommons/hive#19038).
	UnauthorizedStepPostPRGateStatus = "post-pr-gate-status"
	TrustedAuthorReasonAppAuthored   = "trusted-author:app-authored"
	TrustedBotLaneSkipReason         = "trusted-bot-lane"
	WorkflowApprovalRequiredPrefix   = "workflow-approval-required:"
	// WorkflowRunsHeldPrefix: GitHub holds workflow runs on the head behind
	// its approve-workflows gate and the hive did not approve them (its
	// policy does not trust the head, auto-approval is off, or GitHub
	// refused); the rest is the run ids. Only a person's "Approve and run"
	// (or a new head) moves the PR, so it parks at 085 over whatever the
	// lane said next.
	WorkflowRunsHeldPrefix = WorkflowApprovalRequiredPrefix + "held-runs:"
	// RequiredCheckNeverReportedPrefix: auto_merge.required_checks names a
	// check that has never reported on this repo, so the trusted-bot lane
	// cannot gate on it and no other check vouches for the head. A person
	// (or a per-repo required_checks override) resolves it.
	RequiredCheckNeverReportedPrefix = "required-check-never-reported:"
)

// AutoMergeReasonNativeQueueRejectedRepeatedly: GitHub's merge queue failed
// the hive's entry for one head with failed_checks more than once on the
// same base tip. The PR waits at stage 085 so a person sees it; the hive
// retries the head on a slow backoff (NativeQueueRepeatedRecheckFirst, then
// NativeQueueRepeatedRecheck), at once when the head or the base tip
// changes, and at once when a person approves the head after the failure.
const AutoMergeReasonNativeQueueRejectedRepeatedly = AutoMergeReasonNativeQueuePrefix + "-rejected-repeatedly"

// The slow retry of a repeated merge-queue rejection, shared by the lanes
// and the words the dashboard and the 085 comment print. A merge-group
// check that fails for a reason the PR does not carry is usually fixed
// somewhere else (a dependency rebuilt in another repository, an outside
// service back up), and nothing in the PR or its base says when:
//   - NativeQueueRepeatedRecheckFirst (2 hours) after the second failure,
//     enough for a rebuild elsewhere to land;
//   - then NativeQueueRepeatedRecheck (6 hours) after each later one, so a
//     fix is picked up within a working day for about four merge-group
//     runs a day, where the 30-minute loop bought about forty-eight.
const (
	NativeQueueRepeatedRecheckFirst = 2 * time.Hour
	NativeQueueRepeatedRecheck      = 6 * time.Hour
)

// MergeRefusalRecheck is how long a remembered merge refusal (a review or
// merge-method 405, a queue-method refusal, a first merge-queue rejection)
// waits before the lanes try the same head again. It lives here so the
// dashboard and the 085 comment print the same number the lanes use.
const MergeRefusalRecheck = 30 * time.Minute

// Review-conversation refusals: GitHub refused the merge or the merge-queue
// enqueue because the base requires every review conversation resolved
// ("All comments must be resolved").
const (
	// AutoMergeReasonUnresolvedReviewThreads: at least one unresolved
	// thread is not one the hive may resolve itself (a person's or another
	// bot's thread, a hive thread someone replied in, or a hive finding not
	// marked non-blocking). A person resolves them; the lane tries again on
	// a new head or after 30 minutes.
	AutoMergeReasonUnresolvedReviewThreads = "unresolved-review-threads"
	// AutoMergeReasonReviewThreadsResolved: every unresolved thread was the
	// hive's own, marked non-blocking, and the lane resolved them; the next
	// pass merges or enqueues the same head.
	AutoMergeReasonReviewThreadsResolved = "review-threads-resolved"
	// AutoMergeReasonReviewThreadsPending: nothing explained the refusal
	// (the thread read failed, GitHub still counted a thread just resolved,
	// or the resolve calls failed); the lane asks again in 5 minutes, and
	// parks with unresolved-review-threads after 3 such refusals on a head.
	AutoMergeReasonReviewThreadsPending = "review-threads-pending"
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
	// HumanMergeReasonBreakingChange: the PR's title or label marks it a
	// breaking change the owner merges by hand (AutoMergeReasonHumanMergeRequired).
	HumanMergeReasonBreakingChange = "breaking-change"
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

// ReReviewRequestedReasonPrefix: every change request still standing on the
// PR was left on an older commit, and the author has pushed content since.
// GitHub keeps such a review blocking until its reviewer re-reviews or a
// maintainer dismisses it, so the PR waits on the reviewer, not on its
// author. The rest names the reviewers, comma-separated.
const ReReviewRequestedReasonPrefix = "re-review-requested:"

// ReReviewRequestedReason formats the lane reason for reviewers whose change
// requests are on older commits, sorted so the reason is stable across
// sweeps: `re-review-requested:alice,bob`.
func ReReviewRequestedReason(reviewers []string) string {
	var logins []string
	for _, r := range reviewers {
		if r = strings.TrimPrefix(strings.TrimSpace(r), "@"); r != "" && !slices.Contains(logins, r) {
			logins = append(logins, r)
		}
	}
	slices.SortFunc(logins, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return ReReviewRequestedReasonPrefix + strings.Join(logins, ",")
}

// ParseReReviewRequested returns the reviewers a ReReviewRequestedReason
// names. ok is false for any other reason.
func ParseReReviewRequested(reason string) (reviewers []string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(reason), ReReviewRequestedReasonPrefix)
	if !found {
		return nil, false
	}
	for _, r := range strings.Split(rest, ",") {
		if r = strings.TrimSpace(r); r != "" {
			reviewers = append(reviewers, r)
		}
	}
	return reviewers, true
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
	case base == AutoMergeReasonHumanMergePath, base == AutoMergeReasonHumanMergePathEvidence,
		base == AutoMergeReasonSensitivePathDecision, base == AutoMergeReasonSensitivePathEvidence,
		base == AutoMergeReasonSentinelDecision:
		return HumanMergeReasonManualMergePolicy
	case base == AutoMergeReasonHumanMergeRequired:
		return HumanMergeReasonBreakingChange
	case base == AutoMergeReasonMergeRefusedReview, strings.HasPrefix(base, AutoMergeReasonReviewGatePrefix):
		return HumanMergeReasonBranchProtection
	case base == AutoMergeReasonMergeRefusedMethod, base == AutoMergeReasonNativeQueueMethod:
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

// humanMergeReasonWords are the short word and the one-sentence why for
// each HumanMergeReason* code: why a person, not the hive, merges a PR
// parked at 090 (hivecommons/hive#14658). The dashboard's gate cell and the
// 090 entry comment both read them, so the two never disagree. Plain words,
// one short sentence each, no config keys (hivecommons/hive#15394).
var humanMergeReasonWords = map[string][2]string{
	HumanMergeReasonWorkflowFiles:     {"workflow files", "It changes GitHub workflow files, which hive never merges on its own"},
	HumanMergeReasonTestsRemoved:      {"tests removed", "It removes tests, which hive never merges on its own"},
	HumanMergeReasonManualMergePolicy: {"manual-merge policy", "It touches files this repo reserves for a person to merge"},
	HumanMergeReasonBranchProtection:  {"branch protection", "Branch protection wants a review hive cannot give; approve on GitHub or merge it"},
	HumanMergeReasonMergeMethod:       {"merge method", "The repo does not allow the merge method this PR needs"},
	HumanMergeReasonAutoMergeOff:      {"auto-merge off", "Auto-merge is off for this repo, so a person merges"},
	HumanMergeReasonUntrustedAuthor:   {"untrusted author", "Hive won't merge it alone — the author isn't a trusted author yet"},
	HumanMergeReasonUnauthorized:      {"hive bot lacks permission", "The hive bot's GitHub App is missing the permission to merge here; a person merges"},
	HumanMergeReasonReviewerDeferred:  {"reviewer deferred", "A reviewer asked for a person to merge this one"},
	HumanMergeReasonBreakingChange:    {"human merge", "Breaking change — owner merges; hive never merges a ⚠️-titled or needs-owner-merge PR"},
}

// HumanMergeReasonWords returns code's gate-cell word and its one-sentence
// why. ok is false for a code with no words of its own
// (HumanMergeReasonPolicy, "").
func HumanMergeReasonWords(code string) (word, sentence string, ok bool) {
	w, ok := humanMergeReasonWords[code]
	return w[0], w[1], ok
}

// WorkflowRunsHeldReason formats the WorkflowRunsHeldPrefix reason for ids.
func WorkflowRunsHeldReason(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return WorkflowRunsHeldPrefix + strings.Join(parts, ",")
}

// ParseWorkflowRunsHeld returns the run ids a WorkflowRunsHeldPrefix reason
// names; ok is false for any other reason.
func ParseWorkflowRunsHeld(reason string) (ids []string, ok bool) {
	rest, ok := strings.CutPrefix(reason, WorkflowRunsHeldPrefix)
	if !ok {
		return nil, false
	}
	for _, id := range strings.Split(rest, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, true
}

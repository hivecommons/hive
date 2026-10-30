package stageengine

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/cistatus"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type stageDetail = prstage.Detail
type stageObservation = prstage.Observation
type stageVerdict = prstage.Verdict
type ContributorLaneVerdict = prstage.ReviewVerdict

const (
	stageReasonOptionalCheckFailing        = prstage.StageReasonOptionalCheckFailing
	stageRankNone                          = prstage.RankNone
	stageRankRole                          = prstage.RankRole
	stageRankAdmission                     = prstage.RankAdmission
	stageRankOwned                         = prstage.RankOwned
	ReviewGateOutcomeAwaiting              = prstage.ReviewGateOutcomeAwaiting
	ReviewGateOutcomeChangesRequested      = prstage.ReviewGateOutcomeChangesRequested
	ReviewGateOutcomeNoBundle              = prstage.ReviewGateOutcomeNoBundle
	autoMergeReasonBehindBudgetExhausted   = prstage.AutoMergeReasonBehindBudgetExhausted
	autoMergeReasonForkNotEditable         = prstage.AutoMergeReasonForkNotEditable
	autoMergeReasonHumanChangesRequested   = prstage.AutoMergeReasonHumanChangesRequested
	autoMergeReasonHumanMergePath          = prstage.AutoMergeReasonHumanMergePath
	autoMergeReasonHumanMergePathEvidence  = prstage.AutoMergeReasonHumanMergePathEvidence
	autoMergeReasonHumanMergeRequired      = prstage.AutoMergeReasonHumanMergeRequired
	autoMergeReasonIntentEvidence          = prstage.AutoMergeReasonIntentEvidence
	autoMergeReasonIntentUnauthorized      = prstage.AutoMergeReasonIntentUnauthorized
	autoMergeReasonMergeQueueRequired      = prstage.AutoMergeReasonMergeQueueRequired
	autoMergeReasonMergeRefusedMethod      = prstage.AutoMergeReasonMergeRefusedMethod
	autoMergeReasonMergeRefusedReview      = prstage.AutoMergeReasonMergeRefusedReview
	autoMergeReasonNoAppBotLogin           = prstage.AutoMergeReasonNoAppBotLogin
	autoMergeReasonReviewGatePrefix        = prstage.AutoMergeReasonReviewGatePrefix
	contributorLaneReasonAppAuthored       = prstage.ContributorLaneReasonAppAuthored
	contributorLaneReasonAuthorAssociation = prstage.ContributorLaneReasonAuthorAssociation
	contributorLaneReasonChangesRequested  = prstage.ContributorLaneReasonChangesRequested
	contributorLaneReasonClaimAuthor       = prstage.ContributorLaneReasonClaimAuthor
	contributorLaneReasonClaimOtherHive    = prstage.ContributorLaneReasonClaimOtherHive
	contributorLaneReasonClaimTier         = prstage.ContributorLaneReasonClaimTier
	contributorLaneReasonClaimUnverified   = prstage.ContributorLaneReasonClaimUnverified
	contributorLaneReasonFilesEvidence     = prstage.ContributorLaneReasonFilesEvidence
	contributorLaneReasonNoAppBotLogin     = prstage.ContributorLaneReasonNoAppBotLogin
	contributorLaneReasonNoAuthor          = prstage.ContributorLaneReasonNoAuthor
	contributorLaneReasonNoClaim           = prstage.ContributorLaneReasonNoClaim
	contributorLaneReasonNoLinkedIssue     = prstage.ContributorLaneReasonNoLinkedIssue
	contributorLaneReasonNoReviewer        = prstage.ContributorLaneReasonNoReviewer
	contributorLaneReasonNoVerdict         = prstage.ContributorLaneReasonNoVerdict
	contributorLaneReasonReporterTrustHold = prstage.ContributorLaneReasonReporterTrustHold
	contributorLaneReasonSentinelBlocked   = prstage.ContributorLaneReasonSentinelBlocked
	contributorLaneReasonSoaking           = prstage.ContributorLaneReasonSoaking
	contributorLaneReasonTestsRemoved      = prstage.ContributorLaneReasonTestsRemoved
	contributorLaneReasonWorkflowPath      = prstage.ContributorLaneReasonWorkflowPath
	stageLaneContributor                   = prstage.StageLaneContributor
	stageLaneSelf                          = prstage.StageLaneSelf
	trustedAuthorReasonAppAuthored         = prstage.TrustedAuthorReasonAppAuthored
	stageLaneTrusted                       = prstage.StageLaneTrusted
	stageReasonBaseBranchRed               = prstage.StageReasonBaseBranchRed
	stageReasonForkGuard                   = prstage.StageReasonForkGuard
	stageReasonSharedCIIncident            = prstage.StageReasonSharedCIIncident
	trustedBotLaneSkipReason               = prstage.TrustedBotLaneSkipReason
	workflowApprovalRequiredPrefix         = prstage.WorkflowApprovalRequiredPrefix
	stageReasonUnauthorizedPrefix          = prstage.StageReasonUnauthorizedPrefix
)

func normalizeContributorLaneVerdict(v ContributorLaneVerdict) ContributorLaneVerdict {
	return prstage.NormalizeReviewVerdict(v)
}

// CIReasonKind classifies a commitGreenForPR reason.
type CIReasonKind int

const (
	CIReasonNone CIReasonKind = iota
	CIReasonPending
	CIReasonRed
)

// String is the stable name persisted in Record.ActivityDigest
// (/data/stage-labels.json); it must not follow the iota, or an upgrade
// that renumbers the enum would read every PR as active and reset its
// quiet clock.
func (k CIReasonKind) String() string {
	switch k {
	case CIReasonPending:
		return "pending"
	case CIReasonRed:
		return "red"
	}
	return "none"
}

func ClassifyCIReason(reason string) CIReasonKind {
	switch {
	case reason == "":
		return CIReasonNone
	case cistatus.ReasonIsPending(reason), reason == "ci-unverified", reason == "check-pending",
		strings.HasPrefix(reason, "required-check-pending:"), CiReasonInconclusive(reason):
		return CIReasonPending
	case strings.HasPrefix(reason, "required-check-failing:"), strings.HasPrefix(reason, "check-failing:"),
		strings.HasPrefix(reason, "status-"), strings.HasPrefix(reason, "check-"), reason == "check-runs",
		strings.HasPrefix(reason, stageReasonBaseBranchRed):
		return CIReasonRed
	}
	return CIReasonNone
}

// CiReasonInconclusive matches a check that finished without a verdict
// (neutral, skipped). commitGreenForPR never blocks on these, but should a
// reason ever carry one it is "not red": the PR is waiting, not failing.
func CiReasonInconclusive(reason string) bool {
	for _, c := range []string{"neutral", "skipped"} {
		if strings.HasSuffix(reason, "-"+c) {
			return true
		}
	}
	return false
}

// StageReasonVerdictRequiresHuman is StageVerdictReason for requires_human.
const StageReasonVerdictRequiresHuman = "reviewer-verdict:requires_human"

const StageReasonDecisionRequested = "contributor-lane:decision-requested"

// StageReasonDecisionAnswered is the fixing reason when a trusted human's
// `/hive decision <instruction>` answered the decision-requested /
// requires_human verdict on the head: 085 cleared because the person spoke,
// and the author agent is pushing the fix (hivecommons/hive#14679).
const StageReasonDecisionAnswered = "contributor-lane:decision-answered"

// NormalizeStageDetail normalizes the reviewer verdict and folds a decision
// answer it carries into the detail's human approver / change requester, so
// every derivation — lane tracker, startup reconcile, dashboard — reads the
// answered question the same way whichever lane noted it.
func NormalizeStageDetail(d stageDetail) stageDetail {
	if d.VerdictKnown {
		d.Verdict = normalizeContributorLaneVerdict(d.Verdict)
		if v := d.Verdict; v.DecisionAnswered && v.DecisionAnsweredBy != "" && v.DecisionAnsweredHead != "" {
			switch v.DecisionDirective {
			case prstage.DecisionDirectiveApprove:
				if d.HumanApprover == "" || d.HumanApprovalHead != v.DecisionAnsweredHead {
					d.HumanApprover, d.HumanApprovalHead = v.DecisionAnsweredBy, v.DecisionAnsweredHead
				}
			default:
				if d.HumanChangeRequester == "" || d.HumanChangeRequestHead != v.DecisionAnsweredHead {
					d.HumanChangeRequester, d.HumanChangeRequestHead = v.DecisionAnsweredBy, v.DecisionAnsweredHead
				}
			}
		}
	}
	return d
}

// DecisionAnswered reports whether a recorded human decision stands against
// head and the reviewer verdict on that head asked for one.
func DecisionAnswered(d stageDetail, head string) bool {
	d = NormalizeStageDetail(d)
	v := d.Verdict
	return d.VerdictKnown && v.DecisionAnswered && strings.TrimSpace(v.DecisionAnsweredHead) == strings.TrimSpace(head) && VerdictNeedsHuman(d)
}

// StageVerdictReason is the recorded reason when the reviewer verdict, not
// the lane's skip reason, decided the stage.
func StageVerdictReason(d stageDetail) string {
	d = NormalizeStageDetail(d)
	if d.Verdict.DecisionRequested {
		return StageReasonDecisionRequested
	}
	return "reviewer-verdict:" + strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
}

// VerdictBlocks reports whether a known reviewer verdict asks for changes.
func VerdictBlocks(d stageDetail) bool {
	d = NormalizeStageDetail(d)
	if !d.VerdictKnown || d.Verdict.Approved {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
	return v == "changes_requested" || v == "reject" || v == "rejected" || v == "request_changes"
}

// VerdictNeedsHuman reports whether a known reviewer verdict defers the PR to
// a human rather than approving or asking for changes. Only requires_human
// qualifies: it is the one such verdict hive-review can emit
// (approve / changes_requested / requires_human / reject), and the 085 reason
// documented for operators is reviewer-verdict:requires_human.
func VerdictNeedsHuman(d stageDetail) bool {
	d = NormalizeStageDetail(d)
	if !d.VerdictKnown || d.Verdict.Approved {
		return false
	}
	if d.Verdict.DecisionRequested {
		return true
	}
	return strings.ToLower(strings.TrimSpace(d.Verdict.Verdict)) == "requires_human"
}

func TrustedHumanApprovedRequiresHuman(d stageDetail, head string) bool {
	d = NormalizeStageDetail(d)
	return VerdictNeedsHuman(d) && strings.TrimSpace(d.HumanApprover) != "" && strings.TrimSpace(d.HumanApprovalHead) != "" && strings.TrimSpace(d.HumanApprovalHead) == strings.TrimSpace(head)
}

// TrustedHumanChangeRequested reports whether a trusted human's change
// request — a CHANGES_REQUESTED review or a `/hive decision <text>` given
// at 085 — stands against head. It answers the swarm's requires_human /
// decision-requested verdict on that head: the person has spoken, so the
// PR belongs in fixing with the instruction, not parked on the question
// it already answered (hivecommons/hive#14679, seen on #14628).
func TrustedHumanChangeRequested(d stageDetail, head string) bool {
	d = NormalizeStageDetail(d)
	return strings.TrimSpace(d.HumanChangeRequester) != "" && strings.TrimSpace(d.HumanChangeRequestHead) != "" && strings.TrimSpace(d.HumanChangeRequestHead) == strings.TrimSpace(head)
}

// HumanChangeRequestStage is the fixing opinion a standing human change
// request on the head carries: owned rank, reason human-changes-requested,
// whatever the reviewer verdict on that head says.
func HumanChangeRequestStage(o stageObservation) (stageVerdict, bool) {
	d := NormalizeStageDetail(o.Detail)
	if !TrustedHumanChangeRequested(d, o.HeadSHA) {
		return stageVerdict{}, false
	}
	reason := autoMergeReasonHumanChangesRequested
	if DecisionAnswered(d, o.HeadSHA) && d.Verdict.DecisionDirective != prstage.DecisionDirectiveApprove {
		reason = StageReasonDecisionAnswered
	}
	return stageVerdict{Stage: FixingStage(o), Rank: stageRankOwned, Reason: reason}, true
}

func WithHumanApprovalReason(v stageVerdict, reason string) stageVerdict {
	if reason != "" && v.Rank != stageRankNone && !v.Terminal && v.Reason == "" {
		v.Reason = reason
	}
	return v
}

func StageHumanApprovalReason(d stageDetail, head string) string {
	if !TrustedHumanApprovedRequiresHuman(d, head) {
		return ""
	}
	return StageReasonVerdictRequiresHuman + ":human-approved@" + strings.TrimSpace(d.HumanApprover)
}

// stageHandoffReasons are reasons that mean "not this lane's PR".
var stageHandoffReasons = map[string]bool{
	"not-app-authored": true, trustedBotLaneSkipReason: true, "repo-auto-merge-disabled": true,
	trustedAuthorReasonAppAuthored: true, contributorLaneReasonAppAuthored: true, contributorLaneReasonNoAuthor: true, "missing-head-sha": true,
	"fetch-pr": true, "fetch-pr-recheck": true,
}

// stageAdmissionReasons are reasons that mean "this lane cannot vouch for
// the author": the trusted-author lane's role check and the contributor
// lane's registry/claim admission. They yield a WEAK opinion
// (stageRankRole / stageRankAdmission): another lane that admits the author
// owns the stage. The opinion is normally a 090 candidate, which is dropped
// altogether when the resolved tier is trusted (hivecommons/hive#12403 was
// flipped to 090 by the trusted-author lane's role check whenever the
// contributor lane — which admitted the author from the registry — had no
// classified opinion). When the reviewer verdict for the head is already
// requires_human the weak opinion is 085 instead, and that one is kept
// whatever the tier: a person owes a decision, not a merge click
// (hivecommons/hive#13115).
var stageAdmissionReasons = map[string]int{
	"untrusted-author-role":      stageRankRole,
	contributorLaneReasonNoClaim: stageRankAdmission, contributorLaneReasonClaimAuthor: stageRankAdmission,
	contributorLaneReasonClaimOtherHive: stageRankAdmission, contributorLaneReasonNoLinkedIssue: stageRankAdmission,
	contributorLaneReasonClaimTier: stageRankAdmission, contributorLaneReasonClaimUnverified: stageRankAdmission,
	contributorLaneReasonAuthorAssociation: stageRankAdmission,
}

// stageNeedsHumanReasons are reasons that mean the hive will not merge this
// PR unattended whoever the author is: a person owes a DECISION (stage
// 085), not merely a merge click.
var stageNeedsHumanReasons = map[string]bool{
	contributorLaneReasonWorkflowPath: true, contributorLaneReasonTestsRemoved: true,
	contributorLaneReasonNoReviewer: true, contributorLaneReasonReporterTrustHold: true, contributorLaneReasonSentinelBlocked: true,
	contributorLaneReasonNoAppBotLogin: true, contributorLaneReasonFilesEvidence: true,
	autoMergeReasonHumanMergePath: true, autoMergeReasonHumanMergePathEvidence: true,
	autoMergeReasonIntentUnauthorized: true, autoMergeReasonIntentEvidence: true,
	"approval-desk-withheld": true, "author-permission-missing": true, "fork-non-member": true,
	"held": true, "label:hold": true, "label:needs-human": true, "exempt-label": true, "sentinel": true,
	autoMergeReasonNoAppBotLogin: true, "self-authorization-hold-released": true,
	// GitHub refused the merge for a reason no retry cures: branch
	// protection wants a person's approving review, or the repository has
	// the chosen merge method switched off. The merge is not imminent, and
	// a person has to act (approve, change the setting, or merge by hand).
	autoMergeReasonMergeRefusedReview: true, autoMergeReasonMergeRefusedMethod: true,
	// The native queue's merge method would squash away a mixed-authorship
	// PR's signed commits: the same decision as a disabled merge method. It
	// is matched here, before the native-merge-queue prefix keeps the rest
	// at 100.
	prstage.AutoMergeReasonNativeQueueMethod: true,
}

// stageMergeQueueReasons are reasons reached only after every gate passed,
// soak included: the merge is imminent or waiting on GitHub. The soaking
// reason itself is 095 pre-merge-soak (ClassifyStage).
var stageMergeQueueReasons = map[string]bool{
	"head-changed-since-eval": true, "merge-failed": true, "merge-not-applied": true,
	"required-checks-expected": true, "behind": true, "updated-branch": true, "update-branch": true,
	autoMergeReasonBehindBudgetExhausted: true, autoMergeReasonMergeQueueRequired: true,
}

// MergeQueueReason reports whether a lane reason means the head is at the
// merge queue (100): the merge was attempted or is waiting on the queue, a
// branch update, or a release hold — the head itself is ready.
func MergeQueueReason(reason string) bool {
	return stageMergeQueueReasons[reason] || strings.HasPrefix(reason, prstage.AutoMergeReasonNativeQueuePrefix) || strings.HasPrefix(reason, "release-hold")
}

// stageFixingReasons are reasons an agent (or the author) must fix the head.
var stageFixingReasons = map[string]bool{
	"changes-requested": true, autoMergeReasonHumanChangesRequested: true, StageReasonDecisionAnswered: true, contributorLaneReasonChangesRequested: true, "conflicting": true, "not-mergeable": true,
	"label:needs-rebase": true, autoMergeReasonForkNotEditable: true,
}

// FixingStage is the rework stage for the lane: a hive agent reworks its
// own (self-authored lane) PR at 070, while a contributor's PR (trusted-
// author or contributor lane) is handed back to its author with the
// diagnosis — 072 for a clanker, 071 for a human. Before the author kind
// is known (ResolveStage runs on raw observations) a non-self PR reads as
// 071; the apply pass re-derives once stageAuthorKind has run.
func FixingStage(o stageObservation) stagelabels.Stage {
	return stagelabels.FixingStageFor(o.Lane == stageLaneSelf, o.AuthorKind)
}

// ClassifyStage maps one lane observation to a stage opinion. Reason
// strings are the lane constants; see the maps above for the groups.
func ClassifyStage(o stageObservation) stageVerdict {
	o.Detail = NormalizeStageDetail(o.Detail)
	reason := o.Reason
	fixing := FixingStage(o)
	humanApprovalReason := StageHumanApprovalReason(o.Detail, o.HeadSHA)
	if o.Merged && reason == "" {
		return stageVerdict{Terminal: true, Rank: stageRankOwned}
	}
	if reason != "closed" && reason != "gone" && reason != "label-removed" && HasLabelFold(o.Labels, prstage.SupersessionPendingLabel) && HasLabelFold(o.Labels, NeedsHumanLabel) && !HasLabelFold(o.Labels, "hive/keep-open") {
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true, Reason: prstage.ReasonSupersessionPending}
	}
	if o.Err {
		// The lane tripped before deciding. If every author-side gate is
		// already satisfied the PR is waiting on the hive, not the author.
		if v, ok := SharedCIDeferredStage(o); ok {
			return v
		}
		if v, ok := BaseBranchRedStage(o); ok {
			return v
		}
		if v, ok := ForkGuardStage(o); ok {
			return v
		}
		if name := o.Detail.RedCheck(o.HeadSHA); name != "" {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageReasonOptionalCheckFailing + name}
		}
		if AuthorSideDone(o) {
			return stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankRole}
		}
		return stageVerdict{}
	}
	switch reason {
	case "closed", "gone", "label-removed":
		return stageVerdict{Terminal: true, Rank: stageRankOwned}
	case "draft":
		// Not in the pipeline yet: no stage, but clear a stale one.
		return stageVerdict{Stage: stagelabels.None, Rank: stageRankOwned}
	}
	if stageHandoffReasons[reason] {
		return stageVerdict{}
	}
	if strings.HasPrefix(reason, "base-refresh:") {
		// Preparation/queued is not ready-to-merge or a merge train seat.
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned, Reason: reason}
	}
	if train, ok := prstage.ParseMergeTrain(reason); ok {
		// The merge train parked this PR behind an overlapping front
		// (hivecommons/hive#15117): a hive-side wait at the merge gate,
		// never a human stage — unless the train gave up rebasing it.
		if train.State == prstage.MergeTrainStateRebaseExhausted {
			return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
		}
		if train.State == prstage.MergeTrainStateClaimConflict {
			// Another lane's open PR claims the same issue and nobody has
			// decided which lands (hivecommons/hive#15810): a person's
			// call, so 085 — not a turn in the queue.
			return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
		}
		if train.State == prstage.MergeTrainStateDuplicateOf {
			// Two open PRs fix one issue and the queue kept the other
			// (hivecommons/hive#15469): this one goes back to its author
			// — 072 for a relay contributor, 071 for a person, 070 for
			// the App's own — as superseded, never "waiting its turn".
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		// Items that have not passed the review or CI is not green should
		// not be (a) ready for merge, (b) in a merge train
		// (hivecommons/hive#15825): the train seats only heads the lane
		// found green and reviewed, and a seat never outranks what this
		// head's own evidence says.
		if VerdictBlocks(o.Detail) {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: StageVerdictReason(o.Detail)}
		}
		if name := o.Detail.RedCheck(o.HeadSHA); name != "" {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageReasonOptionalCheckFailing + name}
		}
		if train.State == prstage.MergeTrainStateRebasing {
			// The hive is rebasing it: the head it pushes has no CI and no
			// verdicts yet, so it is being fixed (070), not merge-queued.
			return stageVerdict{Stage: stagelabels.Fixing, Rank: stageRankOwned}
		}
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankOwned}, humanApprovalReason)
	}
	if rank := stageAdmissionReasons[reason]; rank != stageRankNone {
		if HasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		if v, ok := HumanChangeRequestStage(o); ok {
			return v
		}
		if v, ok := RequiresHumanStage(o, rank); ok {
			// The reviewer already asked for a person: a 090 candidate would
			// be dropped for the non-approve verdict and fall to 050, where
			// the quiet dwell stalls it (hivecommons/hive#13115). The opinion
			// stays as weak as the admission reason that carries it, so a
			// lane that evaluated the head (conflict, red check) still owns.
			return v
		}
		// Trust is the only thing this lane holds against the PR: a 090
		// candidate, refined by ApplyStageLabels against the head's evidence.
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.ReadyForHumanMerge, Rank: rank, Human: true}, humanApprovalReason)
	}
	if strings.HasPrefix(reason, workflowApprovalRequiredPrefix) {
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
	}
	if strings.HasPrefix(reason, stageReasonUnauthorizedPrefix) {
		// The App lacks the permission the next step needs; only a person
		// can take it (hivecommons/hive#14589). The reason keeps the step.
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
	}
	if reason == autoMergeReasonHumanMergeRequired {
		// A breaking-change title or a needs-owner-merge label
		// (hivecommons/hive#15442): no decision is owed, only the owner's
		// merge click, so the PR parks at 090 with the manual-merge rule
		// once the head is green. Owned, so the trusted tier cannot drop it
		// (ResolveStage keeps only owned 090 opinions for trusted authors).
		if HasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		if v, ok := HumanChangeRequestStage(o); ok {
			return v
		}
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.ReadyForHumanMerge, Rank: stageRankOwned, Human: true}, humanApprovalReason)
	}
	if strings.HasPrefix(reason, "excluded-label:") || stageNeedsHumanReasons[reason] {
		if HasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		if v, ok := HumanChangeRequestStage(o); ok {
			// The "needs a human" the lane parked on — a sentinel alert, a
			// workflow-path fence, a hold — is answered for this head: a
			// trusted maintainer reviewed it and asked for changes, so the
			// author acts next. Without this the sentinel-blocked
			// contributor lane re-derived 085 over the owner's 072 every
			// sweep (hivecommons/hive#15159); a new head re-asks.
			return v
		}
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
	}
	if v, ok := HumanChangeRequestStage(o); ok {
		// The person answered the question on this head: fixing with the
		// instruction, ahead of the decision-requested verdict below.
		return v
	}
	if o.Detail.VerdictKnown && o.Detail.Verdict.DecisionRequested {
		// A posted decision question outranks mergeability: no fixer is kicked
		// for a decision-requested head, and a conflict is moot if the answer
		// is to close it (hivecommons/hive#14607).
		if v, ok := RequiresHumanStage(o, stageRankOwned); ok {
			return v
		}
	}
	if stageFixingReasons[reason] || strings.HasPrefix(reason, "label:") && strings.Contains(reason, "rebase") {
		return stageVerdict{Stage: fixing, Rank: stageRankOwned}
	}
	if VerdictBlocks(o.Detail) {
		// The swarm asked for changes at this head; the lane's own reason
		// (CI pending, no-verdict, …) is not what the author has to act on.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: StageVerdictReason(o.Detail)}
	}
	if v, ok := RequiresHumanStage(o, stageRankOwned); ok {
		// The reviewer DID answer — requires_human / needs-decision — but
		// the lane folds every non-approve verdict into non-mergeable reasons
		// such as no-verdict or pending CI. Hand it to a person instead of
		// leaving it in review-pending while the reviewer skips it as already
		// reviewed (hive #12417/#12438).
		return v
	}
	if v, ok := SharedCIDeferredStage(o); ok {
		// The red is the shared incident's, not the head's.
		return v
	}
	if v, ok := BaseBranchRedStage(o); ok {
		// The red is the base branch's, not the head's.
		return v
	}
	if v, ok := ForkGuardStage(o); ok {
		// The red is the fork guard's: only a maintainer can clear it.
		return v
	}
	if ClassifyCIReason(reason) == CIReasonRed {
		// A red required check is the lane's own diagnosis; it outranks a
		// red optional one in the recorded reason.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned}
	}
	if name := o.Detail.RedCheck(o.HeadSHA); name != "" {
		// The lane gates on required checks only, so it soaks (and merges)
		// past a red optional check and its reason reads as queued or
		// pending. A red check on the head is red CI to its author: the
		// repair stage, with the check named, never 080 or 100.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageReasonOptionalCheckFailing + name}
	}
	if strings.HasPrefix(reason, contributorLaneReasonSoaking) {
		// The lane admitted the head and is sitting out the soak window:
		// 095, not 100 — the merge has not been attempted yet.
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.PreMergeSoak, Rank: stageRankOwned}, humanApprovalReason)
	}
	if MergeQueueReason(reason) {
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.MergeQueue, Rank: stageRankOwned}, humanApprovalReason)
	}
	if reason == contributorLaneReasonNoVerdict {
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}
	}
	if v, ok := ReviewGateStage(reason, fixing); ok {
		return v
	}
	if ClassifyCIReason(reason) == CIReasonPending {
		if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
			return stageVerdict{Stage: stagelabels.Reviewed, Rank: stageRankOwned}
		}
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}
	}
	if AuthorSideDone(o) {
		// Green CI and an approve verdict, yet the lane stopped for a reason
		// no stage claims: admission is pending on the hive's side.
		return WithHumanApprovalReason(stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankRole}, humanApprovalReason)
	}
	return stageVerdict{}
}

// ReviewGateStage is the opinion for a trusted-author head the enforcing
// review gate holds ("review-gate:<outcome>"): no bundle yet or awaiting
// the swarm's approval is 060 review-pending; a swarm changes_requested is
// the author's rework stage. Without it the PR carried no stage at all
// until the verdict was noted — the OWNERS PRs of 2026-10-20 sat unlabelled
// through the whole review wait.
func ReviewGateStage(reason string, fixing stagelabels.Stage) (stageVerdict, bool) {
	if !strings.HasPrefix(reason, autoMergeReasonReviewGatePrefix) {
		return stageVerdict{}, false
	}
	outcome := strings.TrimPrefix(reason, autoMergeReasonReviewGatePrefix)
	switch {
	case outcome == ReviewGateOutcomeNoBundle, outcome == ReviewGateOutcomeAwaiting:
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}, true
	case strings.HasPrefix(outcome, ReviewGateOutcomeChangesRequested):
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stagelabels.LaneReasonChangesRequested}, true
	}
	return stageVerdict{}, false
}

// RequiresHumanStage is the 085 opinion for a head whose reviewer verdict
// is requires_human / needs-decision and that no trusted human has approved
// since; rank is the strength the caller's reason carries.
func RequiresHumanStage(o stageObservation, rank int) (stageVerdict, bool) {
	o.Detail = NormalizeStageDetail(o.Detail)
	if !VerdictNeedsHuman(o.Detail) || TrustedHumanApprovedRequiresHuman(o.Detail, o.HeadSHA) || TrustedHumanChangeRequested(o.Detail, o.HeadSHA) {
		return stageVerdict{}, false
	}
	return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: rank, Human: true, Reason: StageVerdictReason(o.Detail)}, true
}

func PreserveNormalizedFixingStage(prev stagelabels.Record, hadPrev bool, obs stageObservation, verdict stageVerdict) (stageVerdict, bool) {
	if !hadPrev || prev.Stage != stagelabels.Fixing || prev.Reason != stagelabels.LaneReasonChangesRequested {
		return stageVerdict{}, false
	}
	if prev.ActivityHead == "" || obs.HeadSHA == "" || prev.ActivityHead != obs.HeadSHA {
		return stageVerdict{}, false
	}
	if verdict.Stage != stagelabels.NeedsDecision || verdict.Reason != StageReasonVerdictRequiresHuman {
		return stageVerdict{}, false
	}
	if !obs.Detail.VerdictKnown {
		return stageVerdict{}, false
	}
	raw := strings.ToLower(strings.TrimSpace(obs.Detail.Verdict.Verdict))
	normalized := strings.ToLower(strings.TrimSpace(normalizeContributorLaneVerdict(obs.Detail.Verdict).Verdict))
	if raw != "requires_human" || normalized != "changes_requested" {
		return stageVerdict{}, false
	}
	return stageVerdict{Stage: stagelabels.Fixing, Rank: stageRankOwned, Reason: stagelabels.LaneReasonChangesRequested}, true
}

// PreserveStageOnStaleDecisionAnswer keeps a PR parked at 085 when the only
// thing pulling it back to its fixing stage is a trusted human's answered
// decision on a head that has not moved since, and no fixer will act on
// that answer (fixerExcluded: exhausted, no enabled fixer, …). Without it
// the flip to 070 reset the record's dwell and stripped stage/stalled
// ("removed:[stage/stalled] reason:contributor-lane:decision-answered" on
// hive's own #15308, hivecommons/hive#15744) while nobody was fixing.
func PreserveStageOnStaleDecisionAnswer(prev stagelabels.Record, hadPrev bool, obs stageObservation, verdict stageVerdict, fixerExcluded bool) (stageVerdict, bool) {
	if !hadPrev || !fixerExcluded || prev.Stage != stagelabels.NeedsDecision {
		return stageVerdict{}, false
	}
	if verdict.Terminal || verdict.Stage != FixingStage(obs) || verdict.Reason != StageReasonDecisionAnswered {
		return stageVerdict{}, false
	}
	if prev.ActivityHead == "" || obs.HeadSHA == "" || prev.ActivityHead != obs.HeadSHA {
		return stageVerdict{}, false
	}
	return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Reason: prev.Reason}, true
}

// AuthorSideDone reports whether the PR has nothing left for its author to
// do: the reviewer verdict for this head is approve and the lane's reason
// is not a CI state.
func AuthorSideDone(o stageObservation) bool {
	return o.Detail.VerdictKnown && o.Detail.Verdict.Approved && !VerdictBlocks(o.Detail) && ClassifyCIReason(o.Reason) == CIReasonNone
}

func HasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// ResolveStage picks the owning opinion among a PR's lane observations
// (already sorted by lane precedence) and the role tier to show.
func ResolveStage(list []stageObservation) (stageVerdict, stagelabels.Role, stageObservation) {
	var best stageVerdict
	var bestObs stageObservation
	role := stagelabels.RoleNone
	// weakLabelRole is the tier the PR already carries as seen by lanes
	// with no owning opinion this pass.
	weakLabelRole := stagelabels.RoleNone
	for _, o := range list {
		v := ClassifyStage(o)
		if v.Rank > best.Rank {
			best, bestObs = v, o
		}
		if o.AuthorKind == authorkind.Hive || HasLabelFold(o.Labels, "author/hive") {
			role = role.Merge(stagelabels.RoleTrusted)
		}
		if v.Rank < stageRankOwned {
			weakLabelRole = weakLabelRole.Merge(stagelabels.RoleOf(o.Labels))
		}
		// The most specific tier wins: the contributor lane knows
		// registry/claim tiers the trusted-author lane cannot see, so a
		// trusted-author "trusted" is kept but its absence is refined.
		role = role.Merge(o.Detail.Role)
	}
	// A lane that merely cannot vouch for the author (untrusted: no profile,
	// no claim) has nothing to say against the tier another lane wrote on
	// the PR earlier — a maintainer stays role/trusted when only the
	// contributor lane looked this pass. A KNOWN tier a lane read from the
	// registry this pass is different evidence and is kept: it outranks the
	// label a previous pass wrote, so a registry change (promotion or
	// demotion) reaches the PR on the next sweep instead of the stale
	// role/<tier> sticking (hivecommons/hive #14822 vs #14947).
	if role == stagelabels.RoleNone || role == stagelabels.RoleUntrusted {
		role = role.Merge(weakLabelRole)
	}
	// No lane reported a tier this pass (the contributor lane's deep
	// evaluation does not run every cycle): keep the tier the PR already
	// carries instead of dropping it, which flapped role/trusted (#13637).
	if role == stagelabels.RoleNone {
		for _, o := range list {
			role = role.Merge(stagelabels.RoleOf(o.Labels))
		}
	}
	if best.Rank < stageRankOwned {
		switch {
		case role == stagelabels.RoleNone:
			role = stagelabels.RoleUntrusted
		case best.Stage == stagelabels.ReadyForHumanMerge && role == stagelabels.RoleTrusted:
			// Some lane admits this author; a lane that cannot vouch for
			// them has nothing to say about the stage. Leave it as is. A
			// weak 085 (requires_human verdict, #13115) is deliberately
			// kept: the decision is owed whoever the author is.
			best = stageVerdict{}
		}
	}
	if best.Rank == stageRankNone {
		bestObs = list[0]
	}
	return best, role, bestObs
}

// BaseBranchRedReason formats the recorded reason for checks.
func BaseBranchRedReason(checks []string) string {
	return stageReasonBaseBranchRed + strings.Join(checks, ",")
}

// BaseBranchRedStage keeps a red PR whose every failure is the base
// branch's out of the repair stages, exactly as SharedCIDeferredStage does
// for an open incident: CI still pending (050, or 060 with an approve
// verdict), with the checks named in the recorded reason. It only applies
// when the lane's red is CI; changes requested and conflicts stay
// author-facing.
func BaseBranchRedStage(o stageObservation) (stageVerdict, bool) {
	if len(o.BaseRedChecks) == 0 {
		return stageVerdict{}, false
	}
	if ClassifyCIReason(o.Reason) != CIReasonRed && o.Detail.RedCheck(o.HeadSHA) == "" {
		return stageVerdict{}, false
	}
	stage := stagelabels.ReviewPending
	if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
		stage = stagelabels.Reviewed
	}
	return stageVerdict{Stage: stage, Rank: stageRankOwned, Reason: BaseBranchRedReason(o.BaseRedChecks)}, true
}

// ForkGuardReason formats the recorded reason for checks.
func ForkGuardReason(checks []string) string {
	return stageReasonForkGuard + strings.Join(checks, ",")
}

// ForkGuardStage parks a red fork PR whose every failing check refuses it
// only because it is from a fork (a fork-publication or fork-secrets guard)
// at 085 for a maintainer, with the checks named in the recorded reason:
// neither its author nor an in-place adoption can turn such a check green,
// both push to the same fork branch (hivecommons/hive#14273). Like
// BaseBranchRedStage it only applies when the lane's red is CI, and only
// when every red optional check on the head is one of the guard's.
func ForkGuardStage(o stageObservation) (stageVerdict, bool) {
	if len(o.ForkGuardChecks) == 0 {
		return stageVerdict{}, false
	}
	if ClassifyCIReason(o.Reason) != CIReasonRed && o.Detail.RedCheck(o.HeadSHA) == "" {
		return stageVerdict{}, false
	}
	if o.Detail.RedCheck(o.HeadSHA) != "" {
		for _, name := range o.Detail.RedChecks {
			if !slices.Contains(o.ForkGuardChecks, name) {
				return stageVerdict{}, false
			}
		}
	}
	return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true, Reason: ForkGuardReason(o.ForkGuardChecks)}, true
}

// SharedCIDeferredStage keeps a red PR whose failure is the shared incident's
// out of the repair stages: it reads as CI still pending (050, or 060 with an
// approve verdict), with the incident named in the recorded reason. It only
// applies when the lane's red is CI (a required check, or a red optional
// check on the head); changes requested and conflicts stay author-facing.
func SharedCIDeferredStage(o stageObservation) (stageVerdict, bool) {
	if o.SharedCIIncident <= 0 {
		return stageVerdict{}, false
	}
	if ClassifyCIReason(o.Reason) != CIReasonRed && o.Detail.RedCheck(o.HeadSHA) == "" {
		return stageVerdict{}, false
	}
	stage := stagelabels.ReviewPending
	if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
		stage = stagelabels.Reviewed
	}
	return stageVerdict{Stage: stage, Rank: stageRankOwned, Reason: stageReasonSharedCIIncident + strconv.Itoa(o.SharedCIIncident)}, true
}

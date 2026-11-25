package automerge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// observeOnlyReasonRepoAutoMergeDisabled is the lane reason an observe-only
// pass records for a head that reached the merge decision on a repo whose
// auto-merge is off (hivecommons/hive#17566). The stage engine classifies it
// from the head's own evidence, and prstage.HumanMergeReason names its 090
// "auto-merge off".
const observeOnlyReasonRepoAutoMergeDisabled = "repo-auto-merge-disabled"

// observeOnlyBoundary refuses every effect an observe-only lane evaluation
// reaches: merges, branch updates, comments, label and hold changes.
type observeOnlyBoundary struct{}

func (observeOnlyBoundary) Execute(_ context.Context, claim effects.Claim, _ func(context.Context) (effects.Result, error)) (effects.Result, error) {
	return effects.Result{}, effects.NotApplied(fmt.Errorf("observe-only pass on a repo whose auto-merge is off cannot mutate %s", claim.Kind))
}

// observeOnlyEngine is an isolated engine for a lane's observe-only pass. It
// keeps the live policy callbacks and feeds this engine's stage tracker and
// store, but refuses every write: its mutation boundary rejects all effects,
// its observeOnly flag stops the merge, native-queue and merge-train helpers
// and the write-back gate, and its one-per-head marks stay private.
func (c *Engine) observeOnlyEngine() *Engine {
	options := c.integrationReadOnlyOptions()
	options.MutationBoundary = observeOnlyBoundary{}
	options.StageTracker = c.stageTracker
	options.observeOnly = true
	return New(c.transport, options)
}

// observeLaneEval runs one lane's per-PR evaluation of a listed PR on the
// observe-only engine, its prefilter included, and returns the reason the
// lane settled on.
type observeLaneEval func(ctx context.Context, oc *Engine, owner, repoName string, pr *gh.PullRequest) (string, error)

// observeAutoMergeOffRepo is a lane's pass over a repo whose auto-merge is
// off. The lanes used to skip such a repo before recording anything, so its
// PRs never got a stage (hivecommons/hive#17552). Each open PR is evaluated
// as far as the merge decision and recorded for ApplyStageLabels; nothing is
// written to GitHub: the requests run under hgithub.WithIntegrationReadOnly,
// the merge is captured instead of executed, and there is no merge train,
// native-queue reconcile, hold release or workflow approval. A no-op when
// stage labelling is off, since the record is all the pass produces, and
// for a repo that is not one an owner switched off at L6 (repoObservedOnly):
// hives below L6 are not staged.
func (c *Engine) observeAutoMergeOffRepo(ctx context.Context, lane, displayRepo string, targets SweepTargets, eval observeLaneEval) {
	if c == nil || !c.stagePolicy().Enabled || eval == nil || !c.repoObservedOnly(displayRepo) {
		return
	}
	oc := c.observeOnlyEngine()
	ctx = hgithub.WithIntegrationReadOnly(ctx)
	owner, repoName := oc.transport.SplitRepo(displayRepo)
	prs, skipReason, err := oc.openPullRequests(ctx, owner, repoName, targets)
	if err != nil {
		c.warn("observe-only automerge pass could not list open PRs", "lane", lane, "repo", displayRepo, "error", err)
		return
	}
	if skipReason != "" {
		c.debug("observe-only automerge pass skipped repo", "lane", lane, "repo", displayRepo, "reason", skipReason)
		return
	}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() <= 0 {
			continue
		}
		base := pr.GetBase().GetRepo().GetFullName()
		if base == "" {
			base = owner + "/" + repoName
		}
		capture := &integrationEligibilityCapture{repo: base, base: pr.GetBase().GetRef(), head: pr.GetHead().GetSHA(), number: pr.GetNumber()}
		// The capture also disarms the base-preparation and queued-sweep probes (integrationEligibilityProbe), so it is not redundant with observeOnly.
		reason, err := eval(context.WithValue(ctx, integrationEligibilityContextKey{}, capture), oc, owner, repoName, pr)
		if observed := observeOnlyStageReason(lane, reason); observed != reason {
			// The lane got as far as the merge (or a step only a merge
			// would take); its refusal is this pass's, not the head's.
			reason, err = observed, nil
		}
		c.debug("observe-only automerge pass recorded PR", "lane", lane, "repo", displayRepo, "pr", pr.GetNumber(), "reason", reason, "error", err)
		oc.recordStage(lane, displayRepo, owner, repoName, pr, reason, false, err)
	}
}

// observeOnlyStageReason is the reason an observe-only pass records for the
// lane's own: a head that reached the merge decision, or stopped at a step
// that only leads to a merge (the soak, the merge queue, a branch update),
// is recorded as repo-auto-merge-disabled, so it is never staged 080, 095
// or 100 when nothing will merge it. Every other reason is the head's own
// evidence (red or pending CI, a conflict, a hold, an admission or hand-off
// reason) and is kept.
func observeOnlyStageReason(lane, reason string) string {
	if reason == "" || reason == integrationEligibilityCaptured || reason == integrationMemberCaptureUnavailable || reason == integrationMemberIdentityChanged {
		return observeOnlyReasonRepoAutoMergeDisabled
	}
	switch classifyStage(stageObservation{Lane: lane, Reason: reason}).Stage {
	case stagelabels.AwaitingMergeAdmission, stagelabels.PreMergeSoak, stagelabels.MergeQueue:
		return observeOnlyReasonRepoAutoMergeDisabled
	}
	return reason
}

// foldObserveOnlyVerdict gives an observe-only observation the reviewer
// verdict for its head when the drain carried none. A lane notes the
// verdict (noteStageVerdict) before its merge decision but records the
// observation only after the evaluation returns, and the engine ticker and
// the queued sweep both drain the one shared tracker (hivecommons/hive#12573).
// A drain by the other driver in between takes the note. The observation
// then has no verdict unless a later lane in the same pass notes it again —
// the contributor lane does for a trusted author, unless it stops first (repo
// not in the lane, a prefilter or sentinel park, mergeable-unknown, a closing
// or adoption-intent gate, an admission error), and nothing re-notes for the
// App's own PRs. repo-auto-merge-disabled with no verdict classifies as 050,
// so an approved head on an auto-merge-off repo flipped 090 ↔ 050 under the
// same reason. The fold reads the review-verdicts record the lanes read,
// for the observation's own head, so an unreviewed or new head is still 050.
// It reads it through this engine's contributorLaneVerdictFor, which also
// applies a recorded human decision (the observe-only engine has none);
// foldRecordedDecision then skips a head already answered. A trusted bot's
// PR (the self-authored sweep's trusted-bot lane) gets the verdict that lane
// notes on the observe-only engine: the raw record, without a recorded
// decision, passed through trustedBotLaneVerdict, which makes a held verdict
// advisory while auto_merge.trusted_bot_skip_review is on; the decision is
// then foldRecordedDecision's, on both drains.
// Skipping it left an approved mergeraptor PR flipping 050 (this pass, no
// verdict) and 060 or 090 (the lane's noted verdict) on every split drain.
// foldLaneNotedVerdict covers the same race for the lane's own review and
// CI reasons, on a repo whose auto-merge is on or off.
func (c *Engine) foldObserveOnlyVerdict(list []stageObservation) {
	if c == nil || c.contributorLaneVerdict == nil {
		return
	}
	for i := range list {
		o := &list[i]
		if o.Reason != observeOnlyReasonRepoAutoMergeDisabled || o.Detail.VerdictKnown || o.HeadSHA == "" {
			continue
		}
		trustedBot := false
		switch o.Lane {
		case stageLaneTrusted, stageLaneContributor:
		case stageLaneSelf:
			lane, ok := c.sweepLaneForAuthor(o.Author)
			if !ok {
				continue
			}
			trustedBot = lane == laneTrustedBot
		default:
			continue
		}
		var verdict ContributorLaneVerdict
		if trustedBot {
			// The verdict the trusted-bot lane notes (trySweepSelfAuthoredPR)
			// on the observe-only engine, normalized as noteStageVerdict
			// does. That engine has no recorded decisions
			// (integrationReadOnlyOptions), so the lane downgrades the raw
			// verdict before any answer is applied; the answer is left to
			// foldRecordedDecision, as on the drain that kept the note.
			raw := normalizeContributorLaneVerdict(c.contributorLaneVerdict(o.Owner+"/"+o.Repo, o.Number, o.HeadSHA))
			verdict = normalizeContributorLaneVerdict(c.trustedBotLaneVerdict(o.Display, o.Number, o.HeadSHA, laneTrustedBot, raw))
		} else {
			verdict = c.contributorLaneVerdictFor(o.Owner+"/"+o.Repo, o.Number, o.HeadSHA)
		}
		o.Detail.Verdict, o.Detail.VerdictKnown = verdict, true
		o.Detail = normalizeStageDetail(o.Detail)
	}
}

// foldLaneNotedVerdict gives a contributor-lane or trusted-author-lane
// observation the reviewer verdict for its head when the drain carried
// none, for a reason the lane returns only after it noted that verdict
// (laneNotedVerdictReason). It is foldObserveOnlyVerdict's race on a repo
// whose auto-merge is on: the lane notes the verdict (noteStageVerdict) but
// records its observation only once trySweepContributorLanePR or
// trySweepTrustedAuthorPR returns, so a drain by the other driver in
// between takes the note and the next drain holds the observation alone.
// contributor-lane:no-verdict on a requires_human head then classified as
// 050 instead of 085, and the next whole pass routed it back: one PR
// flipped 050 and 085 on one head all night. Because the lane reached the
// note on this pass, the fold reads what the lane noted: the same
// review-verdicts record through contributorLaneVerdictFor, which both
// lanes call, for the observation's own head, so a verdict recorded only
// for an older head is not applied (the head is the sweep listing's; a push
// between the listing and the lane's fetch is corrected by the next
// listing, as in foldObserveOnlyVerdict). An observation recorded with an
// error, or for a reason the lane can also return before the note, is left
// alone, so the fold never gives a verdict the lane did not read.
//
// The split also takes a trusted person's approval of the head, which the
// lane notes beside the verdict (noteStageTrustedHumanApproval) and which
// clears a requires_human verdict. A reason the lane returns only while no
// such approval stands (answerKnown) folds any verdict. Any other reason
// folds a verdict only when it would not route the head to 085 on its own:
// folding the question without the answer would invent an 085 that the
// drain holding both would not derive. A recorded decision is not lost
// that way: contributorLaneVerdictFor applies it, and foldRecordedDecision
// runs next on every drain.
func (c *Engine) foldLaneNotedVerdict(list []stageObservation) {
	if c == nil || c.contributorLaneVerdict == nil {
		return
	}
	for i := range list {
		o := &list[i]
		if o.Detail.VerdictKnown || o.HeadSHA == "" || o.Err {
			continue
		}
		noted, answerKnown := laneNotedVerdictReason(o.Lane, o.Reason)
		if !noted {
			continue
		}
		folded := *o
		folded.Detail.Verdict, folded.Detail.VerdictKnown = c.contributorLaneVerdictFor(o.Owner+"/"+o.Repo, o.Number, o.HeadSHA), true
		folded.Detail = normalizeStageDetail(folded.Detail)
		if _, asks := stageengine.RequiresHumanStage(folded, stageRankOwned); asks && !answerKnown {
			continue
		}
		o.Detail = folded.Detail
	}
}

// laneNotedVerdictReason reports whether lane returns reason only after it
// noted the head's reviewer verdict on this pass (noted), and whether it
// returns it only while no trusted person's approval clears a requires_human
// verdict on the head (answerKnown; see foldLaneNotedVerdict). Only reasons
// whose stage the verdict decides are listed:
//
//   - Contributor lane. evaluateContributorLanePR notes the verdict once
//     admitContributor returns without an error. contributor-lane:no-verdict
//     is returned after the note and only when no approval cleared the
//     verdict (the !humanApproved branch of the review gate), so its answer
//     is known. contributor-lane:soaking (the soak gate) and a CI wait or red
//     check from commitGreenForPR come after the note too, but the approval
//     was read before them. A lane-owned reason never counts as CI here: the
//     adoption-intent gate returns contributor-lane:adoption-intent-pending
//     before the note, and its "-pending" suffix reads as CI pending.
//   - Trusted-author lane. trySweepTrustedAuthorPRAdmitting notes the verdict
//     once the author is admitted. review-gate:<outcome> comes from
//     mergeReviewGate after the note, which holds only when no trusted
//     approval of the head was found, so its answer is known. A CI wait or
//     red check from commitGreenForPR comes after the note.
//
// A merge-train position never counts: both lanes record it before they
// evaluate the head. Reasons left out either place the head without the
// verdict (workflow-path, no-reviewer, review-off, changes-requested) or are
// returned on both sides of the note (an admission reason, conflicting,
// gone), so a drain holding one cannot tell whether the lane read a
// verdict. Keep this in step with the order of the gates in those lanes.
func laneNotedVerdictReason(lane, reason string) (noted, answerKnown bool) {
	// A label prefilter returns before the note, and an operator's label
	// name can end in "-pending" and read as CI.
	if reason == "" || prstage.IsMergeTrainReason(reason) || strings.HasPrefix(reason, "label:") || strings.HasPrefix(reason, "excluded-label:") {
		return false, false
	}
	ci := classifyCIReason(reason) != ciReasonNone
	switch lane {
	case stageLaneContributor:
		switch {
		case reason == contributorLaneReasonNoVerdict:
			return true, true
		case reason == contributorLaneReasonSoaking:
			return true, false
		case ci && !strings.HasPrefix(reason, contributorLaneReasonPrefix):
			return true, false
		}
	case stageLaneTrusted:
		switch {
		case strings.HasPrefix(reason, autoMergeReasonReviewGatePrefix):
			return true, true
		case ci:
			return true, false
		}
	}
	return false, false
}

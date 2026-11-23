package automerge

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
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

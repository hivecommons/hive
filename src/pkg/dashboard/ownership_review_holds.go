package dashboard

import (
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// Review-swarm hold reason codes (hivecommons/hive#12377). A PR the review
// pipeline places in human_hold for a review-swarm reason is your turn for
// any author; review_requires_human (#12329) covers requires_human verdicts.
const (
	// OwnershipReasonReviewRejected: a review verdict rejects the PR.
	OwnershipReasonReviewRejected = "review_rejected"
	// OwnershipReasonReviewDispatchHold: the review dispatcher parked the
	// current head for a person (reviews exhausted their retries, the fix cap
	// was reached, or no fixer agent is available).
	OwnershipReasonReviewDispatchHold = "review_dispatch_hold"
	// OwnershipReasonReviewLoopCap: the review/fix loop reached its cap.
	OwnershipReasonReviewLoopCap = "review_loop_cap"
)

func init() {
	ownershipReasonVerbs[OwnershipReasonReviewRejected] = OwnershipVerbReview
	ownershipReasonVerbs[OwnershipReasonReviewDispatchHold] = OwnershipVerbReview
	ownershipReasonVerbs[OwnershipReasonReviewLoopCap] = OwnershipVerbReview
}

// reviewSwarmState is what the review pipeline reads besides the verdicts:
// the dispatch state (its human holds and fix attempts) and the review-links
// ledger. Ownership reads the same records so the two surfaces agree.
type reviewSwarmState struct {
	dispatch review.DispatchState
	links    map[string]github.ReviewLink
}

// loadReviewSwarm loads the verdict artifact, the dispatch state and the
// review-links ledger /api/review/pipeline reads. A missing file reads as
// empty; an unreadable one is logged and treated as empty.
func (e *ownershipEnv) loadReviewSwarm() {
	if artifact, err := review.LoadArtifact(""); err == nil {
		e.reviewVerdicts = artifact.Items
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ownership: review verdicts unreadable", "error", err)
	}
	if dispatch, err := review.LoadDispatchState(""); err == nil {
		e.reviewSwarm.dispatch = dispatch
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ownership: review dispatch state unreadable", "error", err)
	}
	if links, err := github.LoadReviewLinks(""); err == nil {
		e.reviewSwarm.links = links
	} else {
		slog.Warn("ownership: review links unreadable", "error", err)
	}
}

// reviewHolds returns the review-swarm reasons /api/review/pipeline would put
// the PR in human_hold for, read through pipeline.ReviewHolds so the two
// surfaces cannot disagree.
func (e ownershipEnv) reviewHolds(pr github.PullRequest) []pipeline.ReviewHold {
	if len(e.reviewVerdicts) == 0 && len(e.reviewSwarm.dispatch.Human) == 0 && len(e.reviewSwarm.dispatch.Fixes) == 0 && len(e.reviewSwarm.dispatch.FixAttempts) == 0 {
		return nil
	}
	in := pipeline.Inputs{
		PR:       github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, HiveAuthored: prAgentRole(pr) != ""},
		Verdicts: e.reviewVerdicts,
		Dispatch: e.reviewSwarm.dispatch,
	}
	if link, ok := lookupReviewLink(e.reviewSwarm.links, pr.Repo, reviewPipelineBareRepo(pr.Repo), pr.Number); ok {
		in.ReviewLink = &link
	}
	return pipeline.ReviewHolds(in)
}

// reviewHoldOwnership is the your-turn decision for a PR the review swarm
// holds for a person, for any author (#12377). A Hive PR still inside the
// adjudicator's needs-human wait waits for the adjudicator first; the wait is
// bounded by ownershipAdjudicatorWait.
func (e ownershipEnv) reviewHoldOwnership(pr github.PullRequest, withinAdjudicatorWait bool) (Ownership, bool) {
	if withinAdjudicatorWait {
		return Ownership{}, false
	}
	holds := e.reviewHolds(pr)
	if len(holds) == 0 {
		return Ownership{}, false
	}
	pick := func(kind string) (pipeline.ReviewHold, bool) {
		for _, h := range holds {
			if h.Kind == kind {
				return h, true
			}
		}
		return pipeline.ReviewHold{}, false
	}
	if _, ok := pick(pipeline.ReviewHoldReject); ok {
		return yourTurn(OwnershipReasonReviewRejected, "Hive's review rejected this pull request"), true
	}
	if _, ok := pick(pipeline.ReviewHoldRequiresHuman); ok {
		return yourTurn(OwnershipReasonReviewRequiresHuman, "Hive's review needs a human decision"), true
	}
	if h, ok := pick(pipeline.ReviewHoldDispatch); ok {
		return yourTurn(OwnershipReasonReviewDispatchHold, "Hive's review is parked for a person: "+strings.TrimPrefix(h.Reason, "review dispatch parked for a human: ")), true
	}
	h := holds[0]
	return yourTurn(OwnershipReasonReviewLoopCap, "Hive's review/fix loop stopped: "+h.Reason), true
}

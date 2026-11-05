package dashboard

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

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
	// releases are the persons' releases of verdict holds the self-merge
	// sweep recorded (#12962).
	releases map[string]github.ReviewHoldRelease
	// answer returns the trusted human's recorded answer standing against a
	// PR head (review.DecisionAnswerFor over the decision ledger the
	// un-park sweep writes); nil reads as no answer.
	answer func(repo string, number int, head string) (review.HumanChangeRequest, bool)
}

// ownershipDecisionAnswers is the decision ledger ownership reads; tests swap
// it.
var ownershipDecisionAnswers = func() review.HumanChangeRequestStore { return review.DefaultHumanChangeRequests() }

// decisionAnswerLookup reads the ledger by the PR's full name: snapshot rows
// may carry the bare repository name, the ledger records owner/repo.
func decisionAnswerLookup(org string) func(repo string, number int, head string) (review.HumanChangeRequest, bool) {
	return func(repo string, number int, head string) (review.HumanChangeRequest, bool) {
		full := strings.TrimSpace(repo)
		if !strings.Contains(full, "/") && strings.TrimSpace(org) != "" {
			full = strings.TrimSpace(org) + "/" + full
		}
		return review.DecisionAnswerFor(ownershipDecisionAnswers(), full, number, head)
	}
}

// answeredChangeRequest is the person's instruction that answered the review
// verdict holds on pr's current head: recorded on that head, after every
// verdict hold began, and an instruction rather than a bare approval (an
// approval leaves the merge to the release rules, #12962). A verdict drawn
// after the answer is a new question, so it stands.
func (e ownershipEnv) answeredChangeRequest(pr github.PullRequest, holds []pipeline.ReviewHold) (review.HumanChangeRequest, bool) {
	if e.reviewSwarm.answer == nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return review.HumanChangeRequest{}, false
	}
	req, ok := e.reviewSwarm.answer(pr.Repo, pr.Number, pr.HeadSHA)
	if !ok || req.IsApproval() {
		return review.HumanChangeRequest{}, false
	}
	verdictHeld := false
	for _, h := range holds {
		if !answerableReviewHold(h) {
			continue
		}
		verdictHeld = true
		if !h.Since.IsZero() && !req.RecordedAt.After(h.Since) {
			return review.HumanChangeRequest{}, false
		}
	}
	return req, verdictHeld
}

// answerableReviewHold reports whether a person's answer settles h: a
// requires_human or reject verdict does; the dispatcher's hold and the
// fix-loop cap stay with the owner whatever was answered.
func answerableReviewHold(h pipeline.ReviewHold) bool {
	return h.Kind == pipeline.ReviewHoldRequiresHuman || h.Kind == pipeline.ReviewHoldReject
}

// answeredWaitReason says who has the ball after a person answered the
// review: the author agent is fixing the head.
func answeredWaitReason(req review.HumanChangeRequest) string {
	who := "You"
	if a := strings.TrimSpace(req.Actor); a != "" {
		who = "@" + a
	}
	when := ""
	if !req.RecordedAt.IsZero() {
		when = " at " + req.RecordedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s answered Hive's review on this head%s; the hive is fixing it, and the review comes back to you if the new head needs you", who, when)
}

// answeredPushRefusedReason says the answer is recorded but cannot be
// carried out: the hive cannot push the branch.
func answeredPushRefusedReason(req review.HumanChangeRequest, why string) string {
	who := "You"
	if a := strings.TrimSpace(req.Actor); a != "" {
		who = "@" + a
	}
	return fmt.Sprintf("%s answered Hive's review on this head, but the hive cannot push this branch: %s. Push the fix yourself, ask the author, or close it", who, why)
}

// loadReviewSwarm loads the verdict artifact, the dispatch state, the
// review-links ledger and the review hold releases /api/review/pipeline
// reads. A missing file reads as
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
	if releases, err := github.LoadReviewHoldReleases(""); err == nil {
		e.reviewSwarm.releases = releases
	} else {
		slog.Warn("ownership: review hold releases unreadable", "error", err)
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
	if release, ok := github.ReviewHoldReleaseFor(e.reviewSwarm.releases, pr.Repo, pr.Number); ok {
		in.HoldRelease = &release
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
	// A person already answered the verdict on this head with an
	// instruction (`/hive decision <text>`, a changes-requested review):
	// the ball is with the author agent until it pushes a new head or the
	// swarm asks again. Danathar/dns-failover#155 stayed "Hive's review needs
	// a human decision" for over an hour after the hive moved it to
	// 070-fixing on the owner's answer. A dispatcher hold (fix cap, no
	// fixer) or the loop cap is still the owner's.
	if req, ok := e.answeredChangeRequest(pr, holds); ok {
		// An answer only the hive's fixer could carry out, on a branch the
		// hive cannot push, leaves the PR with the person who answered
		// (projectbluefin/utah#919, projectbluefin/testsuite#1037).
		if e.adoptedPushRefused != nil {
			if why := e.adoptedPushRefused(pr); why != "" {
				return yourTurn(OwnershipReasonAdoptedPushRefused, answeredPushRefusedReason(req, why)), true
			}
		}
		var rest []pipeline.ReviewHold
		for _, h := range holds {
			if !answerableReviewHold(h) {
				rest = append(rest, h)
			}
		}
		if len(rest) == 0 {
			return waitingElsewhereFor(answeredWaitReason(req)), true
		}
		holds = rest
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

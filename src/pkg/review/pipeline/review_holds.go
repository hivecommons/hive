package pipeline

import (
	"fmt"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Review-swarm hold kinds: why the review pipeline, not a label or a person,
// put a PR in human_hold.
const (
	// ReviewHoldRequiresHuman: a requires_human verdict on the current head,
	// or one on an earlier head that no approving verdict has cleared since,
	// that no person has released since either (#12962).
	ReviewHoldRequiresHuman = "requires_human"
	// ReviewHoldReject: a reject verdict, read the same way.
	ReviewHoldReject = "reject"
	// ReviewHoldDispatch: the review dispatcher parked the current head for a
	// person (dispatch exhausted, fix cap reached, no fixer, requires_human).
	ReviewHoldDispatch = "dispatch"
	// ReviewHoldLoopCap: the PR would be fixing or changes_requested with its
	// fix-cycle counter at or over the cap.
	ReviewHoldLoopCap = "loop_cap"
)

// ReviewHold is one reason the review swarm holds a PR for a person.
type ReviewHold struct {
	Kind   string    `json:"kind"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since,omitzero"`
}

// ReviewHolds returns the review-swarm reasons Derive would place the PR in
// human_hold for, ignoring its labels, the queue's held flag, the escalation
// ledger and its merge state, which the caller decides by its own rules.
// /api/review/pipeline and the dashboard's your-turn decision both read it, so
// a review-swarm human_hold card is never "not your turn" (#12377).
func ReviewHolds(in Inputs) []ReviewHold {
	in.PR.Labels = nil
	in.PR.Held = false
	in.Escalation = nil
	in.Merge = MergeState{}
	_, holds := derive(in)
	return holds
}

// reviewSwarmHolds are the verdict and dispatch holds on the PR: a reject or
// requires_human verdict on the current head; failing that, a hold carried
// from an earlier verdict no approving verdict has cleared since (a fix push
// does not clear it, #12374); and the dispatcher's hold for the current head.
//
// A person's release recorded after the verdict (ghpkg.ReleasesReviewHold,
// the rule the self-merge sweep applies) lifts the verdict hold, and with a
// blocking verdict on the current head also the dispatcher's hold, which that
// verdict placed (#12962).
func reviewSwarmHolds(in Inputs, current *review.Aggregate, hold *review.HumanReviewHold) []ReviewHold {
	var out []ReviewHold
	if current != nil {
		switch {
		case current.Verdict == review.VerdictReject:
			out = append(out, ReviewHold{Kind: ReviewHoldReject, Reason: "review verdict rejects the current head", Since: current.FirstRecorded()})
		case RequiresHuman(current):
			out = append(out, ReviewHold{Kind: ReviewHoldRequiresHuman, Reason: "review verdict requires a human", Since: current.FirstRecorded()})
		}
	}
	if len(out) == 0 {
		if carried, ok := carriedVerdictHold(in.Verdicts, in.PR); ok {
			out = append(out, carried)
		}
	}
	if len(out) > 0 && in.HoldRelease != nil && ghpkg.ReleasesReviewHold(out[0].Since, *in.HoldRelease) {
		out = nil
		if blocksOnHead(current) && hold != nil && hold.PlacedByVerdict(*current) {
			hold = nil
		}
	}
	if hold != nil {
		out = append(out, ReviewHold{Kind: ReviewHoldDispatch, Reason: "review dispatch parked for a human: " + nonEmpty(hold.Reason, "no reason recorded"), Since: hold.UpdatedAt})
	}
	return out
}

// blocksOnHead reports whether the current head's verdict requires a human or
// rejects the PR.
func blocksOnHead(current *review.Aggregate) bool {
	return current != nil && (current.Verdict == review.VerdictReject || RequiresHuman(current))
}

// carriedVerdictHold is review.LatestVerdictHold over the PR's verdicts: the
// hold the self-merge sweep enforces, so the board shows what blocks a merge.
func carriedVerdictHold(items []review.Aggregate, pr ghpkg.ReviewQueueEntry) (ReviewHold, bool) {
	var mine []review.Aggregate
	for _, a := range items {
		if samePR(a.Repo, a.Number, pr) {
			mine = append(mine, a)
		}
	}
	h, ok := review.LatestVerdictHold(mine)
	if !ok {
		return ReviewHold{}, false
	}
	kind, what := ReviewHoldRequiresHuman, "required a human"
	if h.Verdict == review.VerdictReject {
		kind, what = ReviewHoldReject, "rejected the pull request"
	}
	where := "the current head"
	if !sameHead(h.HeadSHA, pr.HeadSHA) {
		where = "an earlier head (" + shortSHA(h.HeadSHA) + ")"
	}
	return ReviewHold{Kind: kind, Reason: fmt.Sprintf("review verdict on %s %s; no approving verdict or person's approval since", where, what), Since: h.RecordedAt}, true
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

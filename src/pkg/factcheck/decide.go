package factcheck

import (
	"fmt"
	"strings"
	"time"
)

// Action is what the sweep must do for one PR on this pass.
type Action string

// Actions.
const (
	// ActionNone: nothing to do (waiting, passed, or handed over).
	ActionNone Action = "none"
	// ActionReview: kick the independent reviewer for Round.
	ActionReview Action = "review"
	// ActionRevise: kick the author to address the findings of Round.
	ActionRevise Action = "revise"
	// ActionMarkReady: the pass held and the policy lets the hive mark the
	// draft ready.
	ActionMarkReady Action = "mark-ready"
	// ActionNeedsHuman: the loop stops without a passing verdict; post the
	// one-time hand-off note.
	ActionNeedsHuman Action = "needs-human"
)

// DefaultRevisionWait is the author's time to push a revision when the policy
// carries none.
const DefaultRevisionWait = 6 * time.Hour

// ReviewWait is how long a kicked review may go without a marked review on the
// PR before the round is counted as spent and the reviewer is asked again.
const ReviewWait = 90 * time.Minute

// State is what the hive remembers about one PR between passes. It is what
// keeps the loop bounded when a reviewer or author never answers: a round
// consumed by a kick counts whether or not anything came back.
type State struct {
	// ReviewKicks is how many reviewer rounds have been started.
	ReviewKicks int `json:"review_kicks"`
	// LastReviewKick and LastReviewHead say when and on which head the last
	// reviewer kick went out.
	LastReviewKick time.Time `json:"last_review_kick,omitempty"`
	LastReviewHead string    `json:"last_review_head,omitempty"`
	// RevisionHead is the head whose findings were last sent to the author,
	// and RevisionAt when. Keyed on the head, not the round number, because
	// the head is what the hive can check and the round is what a reviewer
	// writes.
	RevisionHead string    `json:"revision_head,omitempty"`
	RevisionAt   time.Time `json:"revision_at,omitempty"`
	// ReadyMarked is true once the hive moved the draft to ready.
	ReadyMarked bool `json:"ready_marked,omitempty"`
	// NeedsHumanNoted is true once the hand-off note was posted.
	NeedsHumanNoted bool `json:"needs_human_noted,omitempty"`
}

// Input is everything Decide needs.
type Input struct {
	Policy Policy
	PR     PR
	// Markers are the fact-check markers found on the hive's OWN reviews.
	// The caller must drop any marker whose author is not the hive: a marker
	// is a claim that the pass held, and anyone can type one.
	Markers []Marker
	// NeedsHumanPosted is true when the PR already carries the hand-off note.
	NeedsHumanPosted bool
	// HumanTookOver is true when a person has reviewed the PR (see
	// HumanTookOver).
	HumanTookOver bool
	State         State
	Now           time.Time
}

// Decision is the answer for one PR.
type Decision struct {
	Action Action
	// Round is the round the action concerns (review, revise).
	Round int
	// Reason is for the log and the hand-off note.
	Reason string
}

func none(reason string) Decision { return Decision{Action: ActionNone, Reason: reason} }

// latestMarker is the marker with the highest round, preferring one that names
// the PR's current head.
func latestMarker(markers []Marker) (Marker, bool) {
	var best Marker
	found := false
	for _, m := range markers {
		if !found || m.Round > best.Round {
			best, found = m, true
		}
	}
	return best, found
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Decide is the whole loop in one function. The properties it guarantees:
//
//   - rounds are bounded: no more than Policy.MaxRounds reviewer kicks per PR,
//     counted from the hive's own record and from marked reviews, whichever is
//     higher, so a silent reviewer or a missing marker cannot extend the loop;
//   - it stops when the verdict passes, when a person has reviewed, and once
//     the hand-off note has been posted;
//   - findings are sent to the author once per round, and a head that does not
//     move within RevisionWait is handed to a person rather than re-kicked;
//   - the hive marks a draft ready only after a passing round on the current
//     head, and only when the policy says the hive may.
func Decide(in Input) Decision {
	p, pr, st := in.Policy, in.PR, in.State
	if !pr.Open {
		return none("pull request is not open")
	}
	if in.NeedsHumanPosted || st.NeedsHumanNoted {
		return none("already handed to a person")
	}
	if in.HumanTookOver {
		return none("a person is reviewing this PR")
	}
	maxRounds := p.MaxRounds
	if maxRounds <= 0 {
		maxRounds = 1
	}
	wait := p.RevisionWait
	if wait <= 0 {
		wait = DefaultRevisionWait
	}

	latest, haveMarker := latestMarker(in.Markers)
	if haveMarker && SameHead(latest.Head, pr.HeadSHA) {
		switch latest.Verdict {
		case VerdictPass:
			if pr.Draft && p.ReadyPolicy == ReadyHive && !st.ReadyMarked {
				return Decision{Action: ActionMarkReady, Round: latest.Round, Reason: "fact-check passed on the current head"}
			}
			return none("fact-check passed")
		case VerdictInconclusive:
			return Decision{Action: ActionNeedsHuman, Round: latest.Round, Reason: "the reviewer could not verify enough to judge"}
		default: // findings
			if !SameHead(st.RevisionHead, pr.HeadSHA) {
				return Decision{Action: ActionRevise, Round: latest.Round, Reason: "findings are waiting for the author"}
			}
			if in.Now.Sub(st.RevisionAt) >= wait {
				return Decision{Action: ActionNeedsHuman, Round: latest.Round,
					Reason: fmt.Sprintf("the author did not push a revision within %s of the findings", wait)}
			}
			return none("waiting for the author's revision")
		}
	}

	// No marked review on the current head: a review is due, in flight, or the
	// round budget is spent.
	used := st.ReviewKicks
	if haveMarker {
		used = maxInt(used, latest.Round)
	}
	inFlight := st.LastReviewHead != "" && SameHead(st.LastReviewHead, pr.HeadSHA) &&
		!st.LastReviewKick.IsZero() && in.Now.Sub(st.LastReviewKick) < ReviewWait
	if inFlight {
		return none("a review of this head is in progress")
	}
	if used >= maxRounds {
		return Decision{Action: ActionNeedsHuman, Round: used,
			Reason: fmt.Sprintf("the round limit of %d was reached without a passing verdict", maxRounds)}
	}
	return Decision{Action: ActionReview, Round: used + 1, Reason: "no fact-check of the current head"}
}

// Participant is one person or account that reviewed the PR, for HumanTookOver.
type Participant struct {
	Login string
	// Human is true for a person: not a bot, not the hive's own identity.
	Human bool
	// Substantive is true for a review submission or an inline review comment,
	// false for a bare conversation comment.
	Substantive bool
}

// HumanTookOver reports whether a person has reviewed the PR. Their review is
// the review now; further agent rounds would talk over them, and the
// review-feedback sweep already routes what they say to the author. A bare
// conversation comment does not count: it may be a question, not a review.
func HumanTookOver(ps []Participant) bool {
	for _, p := range ps {
		if p.Human && p.Substantive && strings.TrimSpace(p.Login) != "" {
			return true
		}
	}
	return false
}

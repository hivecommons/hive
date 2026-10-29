package delegatedecision

import (
	"strconv"
	"strings"
)

// "Let hive decide" on a PR at stage/090 Ready for human merge
// (hivecommons/hive#15173).
//
// The lanes parked the PR for one reason only: nobody vouches for its
// author. When the reviewer's verdict is good, CI is green and the head is
// mergeable, the person delegating has nothing left to judge — the hive's
// decision is approve, and the merge sweep merges the PR under its own
// number after soak (the same path as "Approve — let hive merge"; the
// approval is the trust override for that head, hivecommons/hive#15136).
// Re-authoring the work into a hive-owned PR is the fallback only when the
// head cannot merge as it is: the review is not good, CI is red, or the PR
// conflicts with its base and needs a rebase the hive cannot do in place.

// Rules DecideHumanMerge returns.
const (
	RuleHumanMergeApprove = "human-merge-approve"
	RuleHumanMergeAdopt   = "human-merge-adopt"
)

// Path words the ✓ hover shows after "hive decided: approve · ".
const (
	pathReviewPassed  = "review passed"
	pathReviewLane    = "review: lane found nothing wrong"
	pathReviewNotGood = "review not good"
	pathCIGreen       = "CI green"
	pathCIRed         = "CI red"
	pathMergeable     = "mergeable"
	pathConflicting   = "conflicting, needs rebase"
	pathDCOMissing    = "sign-off missing"
)

// HumanMergeApprovable reports whether a 090 PR head is one the hive may
// approve for the sweep: review good (or none recorded — at 090 the lane's
// own evaluation already found nothing wrong), CI green, no conflict, no
// missing sign-off.
func HumanMergeApprovable(in Input) bool {
	reviewGood := in.ReviewGood || !in.ReviewKnown
	return reviewGood && in.CIGreen && !in.Conflicting && !in.DCOMissing
}

// DecideHumanMerge is the delegate's answer for a PR at 090. ok=false when
// the input is not a 090 PR.
func DecideHumanMerge(in Input) (Decision, bool) {
	if in.Kind != KindPR || !in.ReadyForHumanMerge {
		return Decision{}, false
	}
	ref := "#" + strconv.Itoa(in.Number)
	if HumanMergeApprovable(in) {
		review := pathReviewPassed
		if !in.ReviewKnown {
			review = pathReviewLane
		}
		path := strings.Join([]string{review, pathCIGreen, pathMergeable}, ", ")
		text := "approve " + ref + ": " + path + " — the merge sweep merges it under its own number after soak"
		return Decision{
			Verdict: VerdictApprove, Rule: RuleHumanMergeApprove, Text: text, Path: path,
			Rationale: "the lanes parked " + ref + " only because nobody vouches for its author; the review, CI and mergeability are good, so the person's delegation is the approval and the hive merges the PR as it is — it is not copied into a hive-owned PR",
			Actions:   []Action{decisionComment(text), {Kind: ActionApproveReview}},
		}, true
	}
	var why []string
	if in.ReviewKnown && !in.ReviewGood {
		why = append(why, pathReviewNotGood)
	}
	if !in.CIGreen {
		why = append(why, pathCIRed)
	}
	if in.Conflicting {
		why = append(why, pathConflicting)
	}
	if in.DCOMissing {
		why = append(why, pathDCOMissing)
	}
	path := strings.Join(why, ", ")
	text := "adopt " + ref + " in supersede mode: " + path + " — the hive re-homes the commits on its own branch, fixes what blocks the merge and opens a superseding PR, preserving the author's commits and sign-off"
	return Decision{
		Verdict: VerdictAdoptSupersede, Rule: RuleHumanMergeAdopt, Text: text, Path: path,
		Rationale: "the head cannot merge as it is (" + path + "); approving would only queue a merge GitHub refuses, so the hive takes the work over",
		Actions:   []Action{decisionComment(text), {Kind: ActionAdoptSupersede}},
	}, true
}

// RunHumanMerge is DecideHumanMerge with the safety rails applied (a held
// PR is refused, like every other delegated decision).
func RunHumanMerge(in Input) (Decision, bool) {
	d, ok := DecideHumanMerge(in)
	if !ok {
		return Decision{}, false
	}
	return ApplyRails(d, Rails{Held: hasLabel(in.Labels, LabelHold)}), true
}

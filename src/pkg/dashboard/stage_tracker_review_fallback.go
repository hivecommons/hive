package dashboard

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// Review-track states the fallback derivation produces for a head with no
// verdict and no reviewer in flight (hivecommons/hive#16661). Each names
// what the row waits on, so the tooltip never reads "activity unknown".
const (
	reviewStateWaitingCI    = "waiting_ci"
	reviewStateNotStarted   = "not_started"
	reviewStateNotRequested = "not_requested"
	reviewStateClosed       = "closed"
	reviewStateUnverified   = "unverified"
	reviewSourceDerived     = "derived-from-pr-state"
)

// stageTrackerReviewFallbackInputs is what the fallback can consult besides
// the review evidence itself: the PR's lifecycle, its CI track on the same
// head and whether a hive reviewer lane is configured for the repo.
type stageTrackerReviewFallbackInputs struct {
	Head         string
	Draft        bool
	Closed       bool
	Merged       bool
	CI           *StageTrackerActivity
	ReviewerLane bool
}

func stageTrackerReviewFallbackInputsFor(it *StageTrackerItem, pr github.PullRequest, reviewerLane bool) stageTrackerReviewFallbackInputs {
	in := stageTrackerReviewFallbackInputs{Head: it.Head, CI: it.CIActivity, ReviewerLane: reviewerLane}
	in.Draft = pr.Draft || slices.Contains(it.Modifiers, modDraft)
	in.Merged = !pr.MergedAt.IsZero()
	in.Closed = in.Merged || !pr.ClosedAt.IsZero() || strings.EqualFold(pr.State, "closed")
	return in
}

// stageTrackerReviewFallbackState replaces a Review track still "unknown"
// after the verdict, dispatch and review-pending derivations with the
// concrete statement the data supports, in this order: the PR is closed or
// merged (review is moot), it is a draft (review not requested), CI on the
// head has not finished (the reviewer waits for it), CI is done but no
// reviewer started (who is next), or nothing at all has been fetched for
// the head — the one case that may still read as no data, and it says the
// next poll refreshes it (hivecommons/hive#16661).
func stageTrackerReviewFallbackState(out *StageTrackerActivity, in stageTrackerReviewFallbackInputs) {
	if out == nil || out.State != "unknown" {
		return
	}
	sha7 := stageTrackerShortSHA(in.Head)
	if sha7 == "" {
		sha7 = "unknown"
	}
	out.Source = reviewSourceDerived
	switch {
	case in.Merged:
		out.State, out.Caption = reviewStateClosed, "Review · n/a (merged)"
		out.Detail = "The PR merged; no further review is requested for this head."
	case in.Closed:
		out.State, out.Caption = reviewStateClosed, "Review · n/a (closed)"
		out.Detail = "The PR is closed; no review is requested for this head."
	case in.Draft:
		out.State, out.Caption = reviewStateNotRequested, "Review · draft, not requested"
		out.Detail = "Review is requested when the author marks the draft ready for review."
	default:
		ci := stageTrackerReviewFallbackCI(in)
		switch ci {
		case "running":
			out.State, out.Caption = reviewStateWaitingCI, "Review · waiting for CI"
			out.Detail = fmt.Sprintf("No review has started on %s: the reviewer is requested once CI reports on this commit.", sha7)
		case "done":
			next := "needs reviewer"
			out.Detail = fmt.Sprintf("CI has reported on %s and no reviewer has picked the head up; this repo has no hive reviewer lane, so a person must review.", sha7)
			if in.ReviewerLane {
				next = "hive reviewer next"
				out.Detail = fmt.Sprintf("CI has reported on %s and no reviewer has picked the head up yet; the hive reviewer is next in line.", sha7)
			}
			out.State, out.Caption = reviewStateNotStarted, "Review · not started · "+next
		default:
			out.Caption = stageTrackerReviewNoDataCaption(in.Head)
			out.Detail = "The spoke has not fetched checks or reviews for this commit yet; the next poll refreshes it."
			out.Source = "unavailable"
		}
	}
}

// stageTrackerReviewFallbackCI classifies the CI track on the same head:
// "running" (checks queued, running or not all reported), "done" (a pass,
// a failure or a skip) or "" when nothing has been observed for the head.
func stageTrackerReviewFallbackCI(in stageTrackerReviewFallbackInputs) string {
	ci := in.CI
	if ci == nil || in.Head == "" || !strings.EqualFold(ci.Head, in.Head) {
		return ""
	}
	switch ci.State {
	case "passing", "failing", "skipped":
		return "done"
	case "running", "queued", "missing":
		return "running"
	}
	if ci.ChecksTotal > 0 {
		if ci.ChecksPassed+ci.ChecksFailed < ci.ChecksTotal {
			return "running"
		}
		return "done"
	}
	return ""
}

// stageTrackerReviewVerdictDetail is the tooltip sentence behind a settled
// verdict on the head: who decided and on how many perspectives, so the
// inactive icon still explains itself. The age rides on since/observed_at
// (the UI renders "12m ago"); the sentence itself is stable across polls.
func stageTrackerReviewVerdictDetail(out *StageTrackerActivity, bundle *evidence.Bundle) string {
	if out == nil || bundle == nil {
		return ""
	}
	verdicts := trackerLatestBundleVerdicts(bundle)
	by := "the hive reviewer"
	n := len(verdicts)
	switch out.State {
	case "approved":
		return fmt.Sprintf("Approved by %s on %s for this head.", by, plural(n, "perspective"))
	case "changes_requested":
		return fmt.Sprintf("Changes requested by %s: %s posted findings on this head; the fix is owed before a re-review.", by, plural(reviewFindingCount(verdicts), "perspective"))
	case "human_decision":
		return fmt.Sprintf("%s posted on this head; the review asks a person to decide.", plural(n, "perspective"))
	}
	return ""
}

func reviewFindingCount(verdicts []evidence.Verdict) int {
	n := 0
	for _, v := range verdicts {
		if v.Verdict == "changes_requested" || v.Verdict == "reject" || v.Verdict == "requires_human" {
			n++
		}
	}
	return n
}

// trackerReviewPerspectives folds the bundle's latest verdicts to one per
// perspective. A blocking verdict outranks an approval from another
// backend on the same perspective, so dissent that still holds the head
// is not hidden; among equals the newest wins.
func trackerReviewPerspectives(bundle *evidence.Bundle) []StageTrackerPerspective {
	latest := map[string]evidence.Verdict{}
	blocking := func(v string) bool { return v != "approve" }
	for _, verdict := range trackerLatestBundleVerdicts(bundle) {
		if verdict.Perspective == "" {
			continue
		}
		previous, ok := latest[verdict.Perspective]
		if !ok || (!blocking(previous.Verdict) && blocking(verdict.Verdict)) ||
			(blocking(previous.Verdict) == blocking(verdict.Verdict) && verdict.RecordedAt.After(previous.RecordedAt)) {
			latest[verdict.Perspective] = verdict
		}
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]StageTrackerPerspective, 0, len(names))
	for _, name := range names {
		v := latest[name]
		p := StageTrackerPerspective{Name: name, Verdict: v.Verdict}
		if len(v.Findings) > 0 {
			p.Summary = v.Findings[0].Summary
		}
		out = append(out, p)
	}
	return out
}

// stageTrackerCINoDataCaption is the CI track's words when no check result
// has been observed for the head: the head and the next step, never
// "unknown" (hivecommons/hive#16661).
func stageTrackerCINoDataCaption(head string) string {
	sha7 := stageTrackerShortSHA(head)
	if sha7 == "" {
		sha7 = "unknown"
	}
	return "CI · no checks observed for head " + sha7 + " yet · refreshing"
}

func stageTrackerReviewNoDataCaption(head string) string {
	sha7 := stageTrackerShortSHA(head)
	if sha7 == "" {
		sha7 = "unknown"
	}
	return "Review · no data for head " + sha7 + " yet · refreshing"
}

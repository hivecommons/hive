package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// hivecommons/hive#15229: a review-pending row whose head the swarm keeps
// passing over says how far the review got and how long it has waited,
// instead of a bare "stalled" pill.
func TestStageTrackerReviewWaitClauseNamesCoverageAndWait(t *testing.T) {
	now := time.Date(2026, 10, 29, 1, 30, 0, 0, time.UTC)
	agg := &review.Aggregate{
		Verdict:      review.VerdictIncomplete,
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveSecurity: review.VerdictApprove, review.PerspectiveIntentAlignment: review.VerdictApprove},
		Reasons:      []string{"review coverage incomplete: missing correctness, style, docs-currency"},
	}
	state := review.DispatchState{Waits: []review.ReviewWaitRecord{{
		Repo: "hivecommons/hive", Number: 15139, HeadSHA: "cf5adb194", Reason: review.ReviewWaitNoFreeReviewer,
		Cycles: review.ReviewWaitStarvationCycles + 1, FirstAt: now.Add(-40 * time.Minute),
	}}}
	got := stageTrackerReviewWaitClause(agg, state, "hivecommons/hive", "hive", 15139, "cf5adb194", now)
	want := "review coverage incomplete: missing correctness, style, docs-currency; waiting for a free reviewer for 4 cycles since 00:50 UTC; promoted ahead of the queue"
	if got != want {
		t.Fatalf("clause = %q, want %q", got, want)
	}

	// The record is keyed by the short repo name on an older state file.
	state.Waits[0].Repo = "hive"
	if got := stageTrackerReviewWaitClause(nil, state, "hivecommons/hive", "hive", 15139, "cf5adb194", now); !strings.HasPrefix(got, "waiting for a free reviewer for 4 cycles") {
		t.Fatalf("short-repo clause = %q", got)
	}

	if got := stageTrackerReviewWaitClause(nil, review.DispatchState{}, "hivecommons/hive", "hive", 15139, "cf5adb194", now); got != "" {
		t.Fatalf("nothing known: clause = %q, want empty", got)
	}
	settled := &review.Aggregate{Verdict: review.VerdictApprove}
	if got := stageTrackerReviewWaitClause(settled, review.DispatchState{}, "hivecommons/hive", "hive", 15139, "cf5adb194", now); got != "" {
		t.Fatalf("settled verdict: clause = %q, want empty", got)
	}
}

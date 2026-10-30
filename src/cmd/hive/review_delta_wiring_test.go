package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

type fakeDeltaDiffer struct {
	calls []string
	diff  review.DeltaDiff
}

func (f *fakeDeltaDiffer) ReviewDeltaDiff(_ context.Context, repo, previousHead, head string) review.DeltaDiff {
	f.calls = append(f.calls, repo+"#"+previousHead+".."+head)
	out := f.diff
	out.PreviousHead = previousHead
	return out
}

func (f *fakeDeltaDiffer) ReviewHeadsSamePatch(context.Context, string, string, string, string) bool {
	return false
}

func (f *fakeDeltaDiffer) ReviewPRChangedFiles(context.Context, string, int) ([]string, bool) {
	return nil, false
}

func writePriorVerdict(t *testing.T, headSHA string, verdict review.Verdict) {
	t.Helper()
	art := review.Artifact{Items: []review.Aggregate{{
		Repo: "hivecommons/hive", Number: 4321, HeadSHA: headSHA, Verdict: verdict,
		MergeEligible: verdict == review.VerdictApprove, RecordedAt: time.Now().UTC().Add(-time.Hour),
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: verdict},
	}}}
	if err := review.WriteArtifact(review.ReviewVerdictsPath, art); err != nil {
		t.Fatal(err)
	}
}

func TestPlanReviewDispatch_DeltaReReviewFetchesCompareAndScopesKick(t *testing.T) {
	redirectReviewPaths(t)
	writePriorVerdict(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", review.VerdictApprove)
	differ := &fakeDeltaDiffer{diff: review.DeltaDiff{Known: true, Files: []review.DeltaFile{{Path: "pkg/x/x.go", Additions: 40}}}}
	plan := planReviewDispatchWith(context.Background(), reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, nil, differ, restoreTestLogger())
	if len(differ.calls) != 1 || differ.calls[0] != "hivecommons/hive#bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.."+reviewTestSHA {
		t.Fatalf("compare calls = %v", differ.calls)
	}
	if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("kicks=%d restamps=%d, want 1/0", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	if k := plan.ReviewKicks[0]; k.DeltaPreviousHead == "" || !strings.Contains(k.Message, "DELTA RE-REVIEW") || !strings.Contains(k.Message, "pkg/x/x.go") {
		t.Fatalf("kick is not a delta re-review: %+v", k)
	}
}

func TestPlanReviewDispatch_DeltaTrivialRestampIsRecorded(t *testing.T) {
	redirectReviewPaths(t)
	writePriorVerdict(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", review.VerdictApprove)
	differ := &fakeDeltaDiffer{diff: review.DeltaDiff{Known: true}}
	plan := planReviewDispatchWith(context.Background(), reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, nil, differ, restoreTestLogger())
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	art, err := review.LoadArtifact("")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := art.AggregateForBody("hivecommons/hive", 4321, reviewTestSHA, "")
	if !ok || got.Verdict != review.VerdictApprove || got.Delta == nil || !got.Delta.Trivial {
		t.Fatalf("restamp not recorded in the artifact: %+v ok=%v", got, ok)
	}
	// The next cycle sees the head as judged: no compare, no kick, no restamp.
	differ.calls = nil
	again := planReviewDispatchWith(context.Background(), reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, nil, differ, restoreTestLogger())
	if len(differ.calls) != 0 || len(again.ReviewKicks) != 0 || len(again.DeltaRestamps) != 0 {
		t.Fatalf("judged head re-planned: calls=%v kicks=%d restamps=%d", differ.calls, len(again.ReviewKicks), len(again.DeltaRestamps))
	}
}

func TestPlanReviewDispatch_DeltaNotFetchedWithoutPriorOrWhenOff(t *testing.T) {
	redirectReviewPaths(t)
	differ := &fakeDeltaDiffer{diff: review.DeltaDiff{Known: true}}
	plan := planReviewDispatchWith(context.Background(), reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, nil, differ, restoreTestLogger())
	if len(differ.calls) != 0 || len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "" {
		t.Fatalf("first review touched the compare API or became a delta: calls=%v kicks=%+v", differ.calls, plan.ReviewKicks)
	}

	writePriorVerdict(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", review.VerdictApprove)
	cfg := reviewSwarmConfig()
	off := false
	cfg.Review.DeltaRereview = &off
	plan = planReviewDispatchWith(context.Background(), cfg, actionableWithPR("hive-bot"), nil, nil, differ, restoreTestLogger())
	if len(differ.calls) != 0 || len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "" || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("delta_rereview: false still planned a delta: calls=%v kicks=%+v restamps=%d", differ.calls, plan.ReviewKicks, len(plan.DeltaRestamps))
	}

	// A nil differ (no GitHub client) falls back to a delta kick without a
	// file list rather than a full review: the prompt alone needs no compare.
	plan = planReviewDispatch(reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, nil, restoreTestLogger())
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead == "" || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("nil differ: kicks=%+v restamps=%d", plan.ReviewKicks, len(plan.DeltaRestamps))
	}
}

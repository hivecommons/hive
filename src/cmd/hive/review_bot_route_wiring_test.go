package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

type fakeChangedFilesLister struct {
	calls int
	files []string
	known bool
}

func (f *fakeChangedFilesLister) ReviewPRChangedFiles(context.Context, string, int) ([]string, bool) {
	f.calls++
	return f.files, f.known
}

func (f *fakeChangedFilesLister) ReviewDeltaDiff(_ context.Context, _, previousHead, _ string) review.DeltaDiff {
	return review.DeltaDiff{PreviousHead: previousHead}
}

func (f *fakeChangedFilesLister) ReviewHeadsSamePatch(context.Context, string, string, string, string) bool {
	return false
}

func TestPlanReviewDispatch_BotAuthorRouteUsesCheapModel(t *testing.T) {
	redirectReviewPaths(t)
	cfg := reviewSwarmConfig()
	cfg.Review.BotAuthorModel = "gpt-6-luna"
	lister := &fakeChangedFilesLister{files: []string{"go.mod", "go.sum"}, known: true}
	plan := planReviewDispatchWith(context.Background(), cfg, actionableWithPR("dependabot[bot]"), nil, nil, lister, restoreTestLogger())
	if lister.calls != 1 || len(plan.ReviewKicks) != 1 {
		t.Fatalf("calls=%d kicks=%d, want 1/1", lister.calls, len(plan.ReviewKicks))
	}
	if k := plan.ReviewKicks[0]; !k.BotAuthorRouted || k.ReviewModel != "gpt-6-luna" {
		t.Fatalf("kick not routed: %+v", k)
	}
}

func TestPlanReviewDispatch_BotAuthorRouteExceptionsAndGates(t *testing.T) {
	redirectReviewPaths(t)
	cfg := reviewSwarmConfig()
	cfg.Review.BotAuthorModel = "gpt-6-luna"

	// A workflow file keeps the full-strength model.
	lister := &fakeChangedFilesLister{files: []string{"go.mod", ".github/workflows/ci.yml"}, known: true}
	plan := planReviewDispatchWith(context.Background(), cfg, actionableWithPR("dependabot[bot]"), nil, nil, lister, restoreTestLogger())
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("workflow change was downgraded: %+v", plan.ReviewKicks)
	}

	// The hive's own App (project.ai_author) is never a "bot author", and
	// its files are not even listed.
	lister = &fakeChangedFilesLister{files: []string{"go.mod"}, known: true}
	plan = planReviewDispatchWith(context.Background(), cfg, actionableWithPR("hive-bot"), nil, nil, lister, restoreTestLogger())
	if lister.calls != 0 || len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("own app routed: calls=%d kicks=%+v", lister.calls, plan.ReviewKicks)
	}

	// Route off: no listing, no downgrade.
	off := reviewSwarmConfig()
	lister = &fakeChangedFilesLister{files: []string{"go.mod"}, known: true}
	plan = planReviewDispatchWith(context.Background(), off, actionableWithPR("dependabot[bot]"), nil, nil, lister, restoreTestLogger())
	if lister.calls != 0 || len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("route off still acted: calls=%d kicks=%+v", lister.calls, plan.ReviewKicks)
	}

	// Listing failed: unknown files, full-strength review.
	lister = &fakeChangedFilesLister{known: false}
	plan = planReviewDispatchWith(context.Background(), cfg, actionableWithPR("dependabot[bot]"), nil, nil, lister, restoreTestLogger())
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("unknown files downgraded: %+v", plan.ReviewKicks)
	}

	// No GitHub client at all (nil lister) behaves the same.
	plan = planReviewDispatch(cfg, actionableWithPR("dependabot[bot]"), nil, nil, restoreTestLogger())
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].BotAuthorRouted {
		t.Fatalf("nil lister downgraded: %+v", plan.ReviewKicks)
	}
}

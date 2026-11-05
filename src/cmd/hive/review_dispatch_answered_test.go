package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// swapReviewPlanChangeRequests points the planner's decision ledger at a
// fresh in-memory store.
func swapReviewPlanChangeRequests(t *testing.T) *review.HumanChangeRequests {
	t.Helper()
	store := review.NewHumanChangeRequests("")
	old := reviewPlanChangeRequests
	reviewPlanChangeRequests = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { reviewPlanChangeRequests = old })
	return store
}

// Danathar/dns-failover#155: hive-wild-mole runs without
// review.require_approval, so the planner returned an empty plan before it
// read the owner's `/hive decision <instruction>`; the PR sat at 070-fixing
// and no agent was kicked. Review stays off for everything else.
func TestPlanReviewDispatchKicksAnsweredHeadWhileReviewIsOff(t *testing.T) {
	for _, toggles := range []struct{ requireApproval, fanOut bool }{{false, true}, {true, false}, {false, false}} {
		redirectReviewPaths(t)
		store := swapReviewPlanChangeRequests(t)
		cfg := reviewSwarmConfig()
		cfg.Review.RequireApproval, cfg.Review.FanOut = toggles.requireApproval, toggles.fanOut
		actionable := actionableWithPR("hive-bot[bot]")
		unanswered := actionable.PRs.Items[0]
		unanswered.Number, unanswered.HeadSHA = 4322, "0ther"
		actionable.PRs.Items = append(actionable.PRs.Items, unanswered)

		if plan := planReviewDispatch(cfg, actionable, nil, nil, restoreTestLogger()); !emptyDispatchPlan(plan) {
			t.Fatalf("%+v: no answer, plan = %+v, want empty", toggles, plan)
		}

		if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 4321, HeadSHA: reviewTestSHA, Source: review.HumanChangeRequestSourceDecision,
			SourceID: 77, Actor: "Danathar", Body: "Fix the symlink case before merge.", RecordedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		plan := planReviewDispatch(cfg, actionable, nil, nil, restoreTestLogger())
		if len(plan.FixKicks) != 1 || plan.FixKicks[0].Number != 4321 || !strings.Contains(plan.FixKicks[0].Message, "Fix the symlink case before merge.") {
			t.Fatalf("%+v: fix kicks = %+v, want one for #4321 carrying the instruction", toggles, plan.FixKicks)
		}
		if len(plan.ReviewKicks) != 0 {
			t.Fatalf("%+v: review kicks = %+v, want none with review dispatch off", toggles, plan.ReviewKicks)
		}

		// A bare approval is not an instruction: nothing to fix.
		store2 := swapReviewPlanChangeRequests(t)
		if err := store2.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 4321, HeadSHA: reviewTestSHA, Source: review.HumanChangeRequestSourceDecision,
			SourceID: 78, Actor: "Danathar", Kind: review.HumanChangeRequestKindApprove, RecordedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if plan := planReviewDispatch(cfg, actionable, nil, nil, restoreTestLogger()); !emptyDispatchPlan(plan) {
			t.Fatalf("%+v: approval-only plan = %+v, want empty", toggles, plan)
		}
	}
}

// With review dispatch off, only answered PRs reach the planner's per-PR
// reads: an unanswered PR whose prior verdict would draw a delta compare
// costs no GitHub call on every cycle.
func TestPlanReviewDispatchAnsweredOnlyReadsNothingForOtherPRs(t *testing.T) {
	redirectReviewPaths(t)
	writePriorVerdict(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", review.VerdictApprove)
	store := swapReviewPlanChangeRequests(t)
	cfg := reviewSwarmConfig()
	cfg.Review.RequireApproval = false
	actionable := actionableWithPR("hive-bot[bot]") // #4321: prior verdict on another head
	answeredPR := actionable.PRs.Items[0]
	answeredPR.Number, answeredPR.HeadSHA = 4322, "cccccccccccccccccccccccccccccccccccccccc"
	actionable.PRs.Items = append(actionable.PRs.Items, answeredPR)
	if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 4322, HeadSHA: answeredPR.HeadSHA, Source: review.HumanChangeRequestSourceDecision,
		SourceID: 79, Actor: "Danathar", Body: "Rename the flag.", RecordedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	differ := &fakeDeltaDiffer{diff: review.DeltaDiff{Known: true}}
	plan := planReviewDispatchWith(context.Background(), cfg, actionable, nil, nil, differ, restoreTestLogger())
	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Number != 4322 {
		t.Fatalf("fix kicks = %+v, want one for #4322", plan.FixKicks)
	}
	if len(differ.calls) != 0 {
		t.Fatalf("compare calls = %v, want none for the unanswered PR", differ.calls)
	}
}

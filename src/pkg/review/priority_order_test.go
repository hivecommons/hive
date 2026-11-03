package review

import (
	"testing"
	"time"
)

// hivecommons/hive#15823: reviewer slots go to merge-ready PRs first, then
// starved heads, then the caller's order; pending-CI heads only take a slot
// that would otherwise go unused.

func kickNumbers(plan DispatchPlan) []int {
	out := make([]int, 0, len(plan.ReviewKicks))
	for _, k := range plan.ReviewKicks {
		out = append(out, k.Number)
	}
	return out
}

func TestOrderByReviewPriorityBandsMergeReadyStarvedAgeThenPendingCI(t *testing.T) {
	pending := dispatchPRNum(1, "sha1")
	pending.CIPending = true
	plain := dispatchPRNum(2, "sha2")
	starvedPR := dispatchPRNum(3, "sha3")
	ready := dispatchPRNum(4, "sha4")
	ready.MergeReady = true
	promoted := []ReviewWaitRecord{{Repo: "acme/hive", Number: 3, HeadSHA: "sha3"}}

	got := orderByReviewPriority([]PullRequest{pending, plain, starvedPR, ready}, promoted, "acme")

	wantOrder := []int{4, 3, 2, 1}
	wantPriority := []string{ReviewPriorityMergeReady, ReviewPriorityStarved, ReviewPriorityAge, ReviewPriorityCIPendingSpare}
	if len(got) != len(wantOrder) {
		t.Fatalf("len = %d, want %d", len(got), len(wantOrder))
	}
	for i, pr := range got {
		if pr.Number != wantOrder[i] || pr.Priority != wantPriority[i] {
			t.Fatalf("pos %d = #%d %q, want #%d %q", i, pr.Number, pr.Priority, wantOrder[i], wantPriority[i])
		}
	}
}

func TestOrderByReviewPriorityKeepsIncomingOrderWithinBand(t *testing.T) {
	a, b, c := dispatchPRNum(7, "sha7"), dispatchPRNum(5, "sha5"), dispatchPRNum(6, "sha6")
	got := orderByReviewPriority([]PullRequest{a, b, c}, nil, "acme")
	if got[0].Number != 7 || got[1].Number != 5 || got[2].Number != 6 {
		t.Fatalf("order changed within band: %+v", kickOrder(got))
	}
}

func kickOrder(prs []PullRequest) []int {
	out := make([]int, 0, len(prs))
	for _, pr := range prs {
		out = append(out, pr.Number)
	}
	return out
}

func TestPlanDispatchMergeReadyPRTakesTheSlotAheadOfOlderAndStarved(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1")},
		Now:                  now,
	}
	ready := dispatchPRNum(4, "sha4")
	ready.MergeReady = true
	// PR 2 is starved, PR 9 is the caller's first pick; the merge-ready PR 4
	// still wins the single reviewer.
	prs := []PullRequest{dispatchPRNum(9, "sha9"), dispatchPRNum(2, "sha2"), ready}
	state := DispatchState{Waits: []ReviewWaitRecord{{Repo: "acme/hive", Number: 2, HeadSHA: "sha2", Reason: ReviewWaitNoFreeReviewer, Cycles: ReviewWaitStarvationCycles, FirstAt: now.Add(-10 * time.Minute)}}}

	plan := PlanDispatch(prs, Artifact{}, state, opts)

	if got := kickNumbers(plan); len(got) != 1 || got[0] != 4 {
		t.Fatalf("kicks = %v, want merge-ready PR 4 only", got)
	}
	if plan.ReviewKicks[0].Priority != ReviewPriorityMergeReady {
		t.Fatalf("priority = %q, want %q", plan.ReviewKicks[0].Priority, ReviewPriorityMergeReady)
	}
}

func TestPlanDispatchPendingCIPROnlyReviewedWithSpareCapacity(t *testing.T) {
	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1")},
	}
	pending := dispatchPRNum(1, "sha1")
	pending.CIPending = true
	prs := []PullRequest{pending, dispatchPRNum(2, "sha2")}

	// One reviewer: the green PR 2 gets it, the pending-CI PR 1 waits.
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, opts)
	if got := kickNumbers(plan); len(got) != 1 || got[0] != 2 {
		t.Fatalf("kicks = %v, want PR 2 (pending CI PR 1 must wait)", got)
	}
	if got := waitingNumbers(plan); got[1] == "" {
		t.Fatalf("pending-CI PR 1 not reported waiting: %+v", plan.Waiting)
	}

	// Two reviewers: the spare slot goes to the pending-CI PR.
	opts.Agents = []AgentCapability{reviewer("r1"), reviewer("r2")}
	plan = PlanDispatch(prs, Artifact{}, DispatchState{}, opts)
	if got := kickNumbers(plan); len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("kicks = %v, want [2 1]", got)
	}
	if plan.ReviewKicks[1].Priority != ReviewPriorityCIPendingSpare {
		t.Fatalf("priority = %q, want %q", plan.ReviewKicks[1].Priority, ReviewPriorityCIPendingSpare)
	}
}

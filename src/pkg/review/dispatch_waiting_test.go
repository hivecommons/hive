package review

import (
	"testing"
	"time"
)

// hivecommons/hive#14685: the planner reports the PRs it could not hand to a
// reviewer so the reviewer autoscaler can size the lane to them.

func waitingNumbers(plan DispatchPlan) map[int]string {
	out := map[int]string{}
	for _, w := range plan.Waiting {
		out[w.Number] = w.Reason
	}
	return out
}

func TestPlanDispatchReportsPRsWaitingForAFreeReviewer(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2"), dispatchPRNum(3, "sha3")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(plan.ReviewKicks) != 2 {
		t.Fatalf("kicks = %d, want 2 (one per reviewer): %+v", len(plan.ReviewKicks), plan.ReviewKicks)
	}
	got := waitingNumbers(plan)
	if len(got) != 1 || got[3] != ReviewWaitNoFreeReviewer {
		t.Fatalf("waiting = %+v, want PR 3 %s", plan.Waiting, ReviewWaitNoFreeReviewer)
	}
	if key := plan.Waiting[0].Key(); key != "acme/hive#3@sha3" {
		t.Fatalf("key = %q", key)
	}
}

func TestPlanDispatchReportsPRsWaitingForAParallelSlot(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   1,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d, want 1", len(plan.ReviewKicks))
	}
	if got := waitingNumbers(plan); len(got) != 1 || got[2] != ReviewWaitNoParallelSlot {
		t.Fatalf("waiting = %+v", plan.Waiting)
	}
}

func TestPlanDispatchNothingWaitingWhenReviewersCover(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(plan.Waiting) != 0 {
		t.Fatalf("waiting = %+v, want none", plan.Waiting)
	}
}

func TestDrainingReviewerGetsNoNewReview(t *testing.T) {
	draining := reviewer("r2")
	draining.Draining = true
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), draining},
	})
	for _, k := range plan.ReviewKicks {
		if k.Agent == "r2" {
			t.Fatalf("draining reviewer was dispatched: %+v", plan.ReviewKicks)
		}
	}
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d, want 1 from r1", len(plan.ReviewKicks))
	}
	if got := waitingNumbers(plan); got[2] != ReviewWaitNoFreeReviewer {
		t.Fatalf("waiting = %+v", plan.Waiting)
	}
}

// hivecommons/hive#15229: a head passed over for lack of a reviewer is
// remembered across cycles, and after ReviewWaitStarvationCycles it walks
// ahead of the PRs the caller put in front of it.

func TestPlanDispatchRecordsConsecutiveWaitCycles(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1")},
		Now:                  now,
	}
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, opts)
	w, ok := plan.State.WaitFor("acme/hive", 2, "sha2")
	if !ok || w.Cycles != 1 || !w.FirstAt.Equal(now) || w.Reason != ReviewWaitNoFreeReviewer {
		t.Fatalf("first cycle wait = %+v (ok=%v), want cycles 1 at %v", w, ok, now)
	}
	if _, ok := plan.State.WaitFor("acme/hive", 1, "sha1"); ok {
		t.Fatalf("dispatched PR 1 must not carry a wait record: %+v", plan.State.Waits)
	}

	// r1 is still busy with PR 1 next cycle; PR 2 waits again.
	opts.Now = now.Add(2 * time.Minute)
	plan = PlanDispatch(prs, Artifact{}, plan.State, opts)
	w, _ = plan.State.WaitFor("acme/hive", 2, "sha2")
	if w.Cycles != 2 || !w.FirstAt.Equal(now) || !w.LastAt.Equal(opts.Now) {
		t.Fatalf("second cycle wait = %+v, want cycles 2 first %v last %v", w, now, opts.Now)
	}

	// PR 1's review landed: its pending entry is gone, r1 is free, PR 2 is
	// dispatched and its wait record is dropped.
	state := plan.State
	state.Pending = removePendingForHead(state.Pending, PullRequest{Repo: "acme/hive", Number: 1, HeadSHA: "sha1"})
	opts.Now = now.Add(4 * time.Minute)
	plan = PlanDispatch([]PullRequest{dispatchPRNum(2, "sha2")}, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != 2 {
		t.Fatalf("kicks = %+v, want PR 2", plan.ReviewKicks)
	}
	if len(plan.State.Waits) != 0 {
		t.Fatalf("waits after dispatch = %+v, want none", plan.State.Waits)
	}
}

func TestPlanDispatchPromotesStarvedHeadAheadOfTheList(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1")},
		Now:                  now,
	}
	// The caller has put the fresh event PR 9 first; PR 2 has already been
	// passed over ReviewWaitStarvationCycles times.
	prs := []PullRequest{dispatchPRNum(9, "sha9"), dispatchPRNum(2, "sha2")}
	state := DispatchState{Waits: []ReviewWaitRecord{{Repo: "acme/hive", Number: 2, HeadSHA: "sha2", Reason: ReviewWaitNoFreeReviewer, Cycles: ReviewWaitStarvationCycles, FirstAt: now.Add(-10 * time.Minute)}}}
	plan := PlanDispatch(prs, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != 2 {
		t.Fatalf("kicks = %+v, want the starved PR 2 first", plan.ReviewKicks)
	}
	if len(plan.Promoted) != 1 || plan.Promoted[0].Number != 2 || plan.Promoted[0].Cycles != ReviewWaitStarvationCycles {
		t.Fatalf("promoted = %+v", plan.Promoted)
	}
	if got := waitingNumbers(plan); len(got) != 1 || got[9] != ReviewWaitNoFreeReviewer {
		t.Fatalf("waiting = %+v, want PR 9", plan.Waiting)
	}
	if _, ok := plan.State.WaitFor("acme/hive", 2, "sha2"); ok {
		t.Fatalf("promoted and dispatched head still has a wait record: %+v", plan.State.Waits)
	}
}

func TestPlanDispatchDoesNotPromoteBelowStarvationThreshold(t *testing.T) {
	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   10,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1")},
		Now:                  time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
	}
	prs := []PullRequest{dispatchPRNum(9, "sha9"), dispatchPRNum(2, "sha2")}
	state := DispatchState{Waits: []ReviewWaitRecord{{Repo: "acme/hive", Number: 2, HeadSHA: "sha2", Reason: ReviewWaitNoFreeReviewer, Cycles: ReviewWaitStarvationCycles - 1}}}
	plan := PlanDispatch(prs, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != 9 {
		t.Fatalf("kicks = %+v, want list order kept (PR 9)", plan.ReviewKicks)
	}
	if len(plan.Promoted) != 0 {
		t.Fatalf("promoted = %+v, want none", plan.Promoted)
	}
	if w, _ := plan.State.WaitFor("acme/hive", 2, "sha2"); w.Cycles != ReviewWaitStarvationCycles {
		t.Fatalf("wait cycles = %d, want %d", w.Cycles, ReviewWaitStarvationCycles)
	}
}

func TestPruneStateDropsWaitRecordsForClosedHeads(t *testing.T) {
	state := DispatchState{Waits: []ReviewWaitRecord{
		{Repo: "acme/hive", Number: 2, HeadSHA: "sha2", Cycles: 2},
		{Repo: "acme/hive", Number: 3, HeadSHA: "old", Cycles: 5},
	}}
	got := pruneState(state, []PullRequest{dispatchPRNum(2, "sha2"), dispatchPRNum(3, "new")}, "acme")
	if len(got.Waits) != 1 || got.Waits[0].Number != 2 {
		t.Fatalf("waits = %+v, want only PR 2 on its open head", got.Waits)
	}
}

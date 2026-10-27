package review

import (
	"testing"
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

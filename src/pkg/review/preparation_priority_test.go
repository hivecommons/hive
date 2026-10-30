package review

import (
	"testing"
	"time"
)

func TestPreparationOwnerGetsNextFreeSlotWithoutInterruptingValidReviews(t *testing.T) {
	now := time.Now().UTC()
	owner := dispatchPRNum(15868, "refreshed")
	owner.MergeReady, owner.PreparationOwner = true, true
	other := dispatchPRNum(15819, "other")
	other.MergeReady = true
	opts := DispatchOptions{RequireApproval: true, FanOut: true, CombinedPerspectives: true,
		ProjectOrg: "acme", MaxParallelReviews: 1, Agents: []AgentCapability{reviewer("r1")}, Now: now}
	// A stale pre-refresh bundle must neither settle nor receive the kick.
	artifact := Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: owner.Number, HeadSHA: "old", Verdict: VerdictApprove}}}
	state := DispatchState{Waits: []ReviewWaitRecord{{Repo: "acme/hive", Number: other.Number,
		HeadSHA: other.HeadSHA, Cycles: ReviewWaitStarvationCycles}}}
	plan := PlanDispatch([]PullRequest{other, owner}, artifact, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != owner.Number ||
		plan.ReviewKicks[0].HeadSHA != "refreshed" || plan.ReviewKicks[0].Priority != ReviewPriorityPreparation {
		t.Fatalf("convoy handoff kicks=%+v", plan.ReviewKicks)
	}
	// The same preferred owner cannot preempt a valid review already running.
	busy := PlanDispatch([]PullRequest{other}, Artifact{}, DispatchState{}, opts)
	plan = PlanDispatch([]PullRequest{other, owner}, artifact, busy.State, opts)
	if len(plan.ReviewKicks) != 0 || len(plan.State.Pending) != len(busy.State.Pending) {
		t.Fatalf("busy valid reviewer interrupted: %+v", plan)
	}
}

func TestPreparationHandoffYieldsToAgedWorkAfterConfirmedServiceAcrossOwners(t *testing.T) {
	owner := dispatchPRNum(7, "owner1")
	owner.MergeReady, owner.PreparationOwner = true, true
	aged := dispatchPRNum(2, "aged")
	aged.MergeReady = true
	opts := DispatchOptions{RequireApproval: true, FanOut: true, CombinedPerspectives: true,
		ProjectOrg: "acme", MaxParallelReviews: 1, Agents: []AgentCapability{reviewer("r1")}}
	state := DispatchState{Waits: []ReviewWaitRecord{{Repo: "acme/hive", Number: aged.Number,
		HeadSHA: aged.HeadSHA, Cycles: ReviewWaitStarvationCycles}}}
	plan := PlanDispatch([]PullRequest{aged, owner}, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != owner.Number {
		t.Fatalf("first handoff=%+v", plan.ReviewKicks)
	}
	// Only delivered service counts. Suppressed kicks cannot consume a turn.
	undelivered := ConfirmDelivered(plan.State, plan.ReviewKicks, nil)
	if lastReviewWasPreparation(undelivered.Recent) {
		t.Fatal("undelivered preparation kick consumed service")
	}
	state = ConfirmDelivered(plan.State, plan.ReviewKicks, plan.ReviewKicks)
	state.Recent = append(state.Recent, RecentReview{Repo: "acme/hive", Number: 99,
		HeadSHA: "ordinary", Confirmed: time.Now().UTC(), Priority: ReviewPriorityMergeReady})
	state.Pending = nil // the first owner's review finished; the next owner is fresh
	owner.Number, owner.HeadSHA = 8, "owner2"
	plan = PlanDispatch([]PullRequest{owner, aged}, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != aged.Number ||
		plan.ReviewKicks[0].Priority != ReviewPriorityPreparationYield {
		t.Fatalf("fresh owners starved aged work: %+v", plan.ReviewKicks)
	}

	state = ConfirmDelivered(plan.State, plan.ReviewKicks, plan.ReviewKicks)
	state.Pending = nil
	plan = PlanDispatch([]PullRequest{owner}, Artifact{}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != owner.Number {
		t.Fatalf("owner did not regain service after fairness yield: %+v", plan.ReviewKicks)
	}
}

func TestUnrelatedDeliveriesCannotClearPreparationFairnessYield(t *testing.T) {
	now := time.Now().UTC()
	for _, delay := range []time.Duration{0, time.Second} {
		for _, ordinaryFirst := range []bool{false, true} {
			prepared := RecentReview{Confirmed: now, Priority: ReviewPriorityPreparation}
			ordinary := RecentReview{Confirmed: now.Add(delay), Priority: ReviewPriorityMergeReady}
			recent := []RecentReview{prepared, ordinary}
			if ordinaryFirst {
				recent = []RecentReview{ordinary, prepared}
			}
			if !lastReviewWasPreparation(recent) {
				t.Fatal("unrelated delivery canceled owed fairness yield")
			}
			recent = append(recent, RecentReview{Confirmed: now.Add(delay), Priority: ReviewPriorityPreparationYield})
			if lastReviewWasPreparation(recent) {
				t.Fatal("confirmed aged service did not pay fairness yield")
			}
		}
	}
}

func TestPreparationPriorityDoesNotPromotePendingCIOrChangeDefaultOrdering(t *testing.T) {
	a, b := dispatchPRNum(1, "one"), dispatchPRNum(2, "two")
	a.MergeReady, b.MergeReady = true, true
	b.PreparationOwner, b.CIPending = true, true
	got := orderByReviewPriority([]PullRequest{a, b}, nil, "acme")
	if got[0].Number != a.Number || got[1].Priority == ReviewPriorityPreparation {
		t.Fatalf("pending-CI owner promoted: %+v", got)
	}
	b.PreparationOwner, b.CIPending = false, false
	got = orderByReviewPriority([]PullRequest{a, b}, nil, "acme", true)
	if got[0].Number != a.Number || got[0].Priority != ReviewPriorityMergeReady {
		t.Fatalf("no-owner ordering changed: %+v", got)
	}
}

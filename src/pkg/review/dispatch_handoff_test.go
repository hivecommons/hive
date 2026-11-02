package review

import (
	"testing"
	"time"
)

func TestDispatchIncompleteCoverageIsBoundToCurrentBody(t *testing.T) {
	now := time.Now().UTC()
	pr := dispatchPR("sha1")
	pr.Repo, pr.BodyDigest = "acme/hive", "current-body"
	old := incompleteAggregate(PerspectiveCorrectness)
	old.BodyDigest = "old-body"
	opts := DispatchOptions{RequireApproval: true, FanOut: true, Now: now, MaxParallelReviews: 1,
		Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}
	state := DispatchState{Pending: []PendingReview{{
		Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest,
		Perspective: PerspectiveSecurity, Agent: "r1", Dispatched: now.Add(-time.Minute),
	}}}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{old}}, state, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveCorrectness ||
		plan.ReviewKicks[0].BodyDigest != pr.BodyDigest || plan.ReviewKicks[0].Agent != "r2" {
		t.Fatalf("old-body approval hid current correctness or duplicated the busy worker: %+v", plan)
	}
	if len(plan.State.Pending) != 2 || !plan.State.Pending[0].Dispatched.Equal(state.Pending[0].Dispatched) {
		t.Fatalf("current security request was lost or restarted: %+v", plan.State.Pending)
	}
}

func TestDispatchCollectsCompletedWorkersBeforePriorityWalk(t *testing.T) {
	now := time.Now().UTC()
	first := dispatchPR("head-first")
	first.Repo, first.Number, first.BodyDigest, first.MergeReady = "acme/hive", 1, "body-first", true
	later := dispatchPR("head-later")
	later.Repo, later.Number, later.BodyDigest = "acme/hive", 2, "body-later"
	agg := Aggregate{Repo: later.Repo, Number: later.Number, HeadSHA: later.HeadSHA,
		BodyDigest: later.BodyDigest, Verdict: VerdictIncomplete, RecordedAt: now,
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}}
	state := DispatchState{Pending: []PendingReview{{
		Repo: later.Repo, Number: later.Number, HeadSHA: later.HeadSHA, BodyDigest: later.BodyDigest,
		Perspective: PerspectiveCorrectness, Agent: "r1", Dispatched: now.Add(-time.Minute),
	}}}
	plan := PlanDispatch([]PullRequest{first, later}, Artifact{Items: []Aggregate{agg}}, state, DispatchOptions{
		RequireApproval: true, FanOut: true, Now: now, MaxParallelReviews: 1,
		Agents: []AgentCapability{reviewer("r1")},
	})
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Number != first.Number {
		t.Fatalf("completed later work falsely occupied the only reviewer: %+v", plan)
	}
}

func TestCompletedReviewCannotReleaseAnotherIdentity(t *testing.T) {
	now := time.Now().UTC()
	pending := PendingReview{Repo: "acme/hive", Number: 7, HeadSHA: "head", BodyDigest: "current",
		Perspective: PerspectiveCorrectness, Agent: "r1", Head: "model-a", Dispatched: now}
	matching := Aggregate{Repo: pending.Repo, Number: pending.Number, HeadSHA: pending.HeadSHA,
		BodyDigest: pending.BodyDigest, RecordedAt: now.Add(time.Second),
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove},
		Heads:        map[Perspective][]string{PerspectiveCorrectness: {"model-a"}}}
	tests := []struct {
		name   string
		change func(*Aggregate)
	}{
		{"older body", func(a *Aggregate) { a.BodyDigest = "old" }},
		{"older head", func(a *Aggregate) { a.HeadSHA = "old" }},
		{"older result", func(a *Aggregate) { a.RecordedAt = now.Add(-time.Second) }},
		{"other review head", func(a *Aggregate) {
			a.Heads = map[Perspective][]string{PerspectiveCorrectness: {"model-b"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := matching
			tt.change(&agg)
			if got := removeCompletedReviews([]PendingReview{pending}, Artifact{Items: []Aggregate{agg}}); len(got) != 1 {
				t.Fatal("an incompatible completion released the active reviewer")
			}
		})
	}
	pending.Decision = true
	if got := removeCompletedReviews([]PendingReview{pending}, Artifact{Items: []Aggregate{matching}}); len(got) != 1 {
		t.Fatal("raw head reports released a decision worker")
	}
	matching.Decided = true
	if got := removeCompletedReviews([]PendingReview{pending}, Artifact{Items: []Aggregate{matching}}); len(got) != 0 {
		t.Fatal("matching decided result did not release its worker")
	}
}

func TestDispatchDoesNotCombineBodyDigests(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.Repo, pr.BodyDigest = "acme/hive", "current"
	current := incompleteAggregate(PerspectiveStyle, PerspectiveDocsCurrency)
	current.BodyDigest = pr.BodyDigest
	old := incompleteAggregate(PerspectiveCorrectness, PerspectiveSecurity, PerspectiveIntentAlignment)
	old.BodyDigest = "old"
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{old, current}}, DispatchState{}, DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 3,
		Agents: []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")},
	})
	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("want exactly three missing current-body perspectives, got %+v", plan.ReviewKicks)
	}

	for _, kick := range plan.ReviewKicks {
		if kick.BodyDigest != pr.BodyDigest || kick.Perspective == PerspectiveStyle || kick.Perspective == PerspectiveDocsCurrency {
			t.Fatalf("re-requested current coverage or used a stale body: %+v", kick)
		}
	}

}

func TestOldBodyWorkDoesNotCoverCurrentBodyOrFreeItsBusyWorker(t *testing.T) {
	now := time.Now().UTC()
	pr := dispatchPR("sha1")
	pr.Repo, pr.BodyDigest = "acme/hive", "current"
	state := DispatchState{Pending: []PendingReview{{
		Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: "old",
		Perspective: PerspectiveCorrectness, Agent: "r1", Dispatched: now,
	}}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, DispatchOptions{
		RequireApproval: true, FanOut: true, Now: now, MaxParallelReviews: 1,
		Agents: []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveCorrectness || plan.ReviewKicks[0].Agent != "r2" {
		t.Fatalf("old work supplied coverage or its still-busy worker was reused: %+v", plan)
	}
}

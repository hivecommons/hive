package review

import (
	"testing"
	"time"
)

// Regression tests for hivecommons/hive#14361: pending entries that no longer
// stand for work in progress must not keep a reviewer "busy" under the
// one-kick-in-flight rule, or the swarm stops dispatching for every PR.

func busyTestOpts(now time.Time) DispatchOptions {
	opts := headsOpts()
	opts.Now = now
	opts.PendingTTL = 45 * time.Minute
	base := reviewer("cr")
	base.HasReviewHeads = true
	opts.Agents = []AgentCapability{
		base,
		headReviewer("cr-sonnet-1", "cr-sonnet", "claude", "claude-sonnet-5-5"),
		headReviewer("cr-sonnet-2", "cr-sonnet", "claude", "claude-sonnet-5-5"),
	}
	return opts
}

func pinnedPending(number int, sha string, dispatched time.Time) []PendingReview {
	return []PendingReview{
		{Repo: "acme/hive", Number: number, HeadSHA: sha, Perspective: PerspectiveCorrectness, Head: "cr-sonnet", Agent: "cr-sonnet-1", Dispatched: dispatched},
		{Repo: "acme/hive", Number: number, HeadSHA: sha, Perspective: PerspectiveSecurity, Head: "cr-sonnet", Agent: "cr-sonnet-2", Dispatched: dispatched},
	}
}

func kicksFor(plan DispatchPlan, number int) int {
	n := 0
	for _, k := range plan.ReviewKicks {
		if k.Number == number {
			n++
		}
	}
	return n
}

// A head that ran out of redispatches keeps its pending entries (so it is
// never re-kicked) and is held for a human. Those entries used to pin every
// replica forever; the next PR must still be reviewed.
func TestExhaustedHeadDoesNotKeepReviewersBusy(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := busyTestOpts(now)
	state := DispatchState{
		Pending: pinnedPending(8, "held8", now.Add(-36*time.Hour)),
		Human: []HumanReviewHold{{Repo: "acme/hive", Number: 8, HeadSHA: "held8",
			Reason: HoldReasonDispatchExhausted + ": 3 review dispatches for this head ended without a verdict", UpdatedAt: now.Add(-30 * time.Hour)}},
	}
	prs := []PullRequest{dispatchPRNum(8, "held8"), dispatchPRNum(7, "sha1")}
	plan := PlanDispatch(prs, Artifact{}, state, opts)

	if got := kicksFor(plan, 7); got != 2 {
		t.Fatalf("PR 7 kicks = %d, want 2 (both replicas free): %+v", got, plan.ReviewKicks)
	}
	if got := kicksFor(plan, 8); got != 0 {
		t.Fatalf("exhausted PR 8 was re-kicked: %+v", plan.ReviewKicks)
	}
	kept := 0
	for _, p := range plan.State.Pending {
		if p.Number == 8 {
			kept++
		}
	}
	if kept != 2 {
		t.Fatalf("exhausted head's pending entries = %d, want 2 kept as its record", kept)
	}
}

// Entries older than pending_ttl whose PR the planner has not reached yet in
// this cycle must not block an earlier PR either.
func TestExpiredPendingOnAnotherPRDoesNotBlock(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := busyTestOpts(now)
	state := DispatchState{Pending: pinnedPending(9, "sha9", now.Add(-2*time.Hour))}
	// PR 7 comes first, so PR 9's stale entries are still in state while it is planned.
	prs := []PullRequest{dispatchPRNum(7, "sha1"), dispatchPRNum(9, "sha9")}
	plan := PlanDispatch(prs, Artifact{}, state, opts)
	if got := kicksFor(plan, 7); got != 2 {
		t.Fatalf("PR 7 kicks = %d, want 2: %+v", got, plan.ReviewKicks)
	}
}

// A live entry (inside pending_ttl) still makes its agent busy.
func TestLivePendingStillMakesTheAgentBusy(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := busyTestOpts(now)
	state := DispatchState{Pending: pinnedPending(9, "sha9", now.Add(-5*time.Minute))}
	plan := PlanDispatch([]PullRequest{dispatchPRNum(7, "sha1"), dispatchPRNum(9, "sha9")}, Artifact{}, state, opts)
	if got := kicksFor(plan, 7); got != 0 {
		t.Fatalf("PR 7 got %d kicks while both replicas are mid-review: %+v", got, plan.ReviewKicks)
	}
}

// Lowering a head's replicas leaves entries naming agents that no longer
// exist; they are pruned and the perspective is asked again.
func TestPendingForRemovedAgentIsPruned(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := busyTestOpts(now)
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Head: "cr-sonnet", Agent: "cr-sonnet-5", Dispatched: now.Add(-5 * time.Minute)},
	}}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, state, opts)
	for _, p := range plan.State.Pending {
		if p.Agent == "cr-sonnet-5" {
			t.Fatalf("entry for removed agent kept: %+v", p)
		}
	}
	asked := false
	for _, k := range plan.ReviewKicks {
		if k.Perspective == PerspectiveCorrectness {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("correctness not re-asked after its removed agent's entry was pruned: %+v", plan.ReviewKicks)
	}
}

// Once the human hold on an exhausted head is cleared, its stale entries
// expire and the head is asked again.
func TestClearedHoldRedispatchesTheHead(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := busyTestOpts(now)
	state := DispatchState{Pending: pinnedPending(8, "held8", now.Add(-36*time.Hour))}
	plan := PlanDispatch([]PullRequest{dispatchPRNum(8, "held8")}, Artifact{}, state, opts)
	if got := kicksFor(plan, 8); got == 0 {
		t.Fatalf("PR 8 not re-dispatched after its hold was cleared: %+v", plan.ReviewKicks)
	}
}

// Without review heads, a plain reviewer pinned only by an exhausted head's
// entry is free for other work.
func TestNoHeadsExhaustedEntryDoesNotBlock(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	opts := DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 5, ProjectOrg: "acme",
		Agents: []AgentCapability{reviewer("r1")}, Now: now, PendingTTL: 45 * time.Minute}
	state := DispatchState{
		Pending: []PendingReview{{Repo: "acme/hive", Number: 8, HeadSHA: "held8", Perspective: PerspectiveCorrectness, Agent: "r1", Dispatched: now.Add(-10 * time.Hour)}},
		Human: []HumanReviewHold{{Repo: "acme/hive", Number: 8, HeadSHA: "held8",
			Reason: HoldReasonDispatchExhausted + ": 3 review dispatches for this head ended without a verdict"}},
	}
	plan := PlanDispatch([]PullRequest{dispatchPRNum(8, "held8"), dispatchPRNum(7, "sha1")}, Artifact{}, state, opts)
	if got := kicksFor(plan, 7); got != 1 {
		t.Fatalf("PR 7 kicks = %d, want 1 on the freed r1: %+v", got, plan.ReviewKicks)
	}
}

// A head held for a human after running out of redispatches is labelled
// needs-human, not "reviewing" forever.
func TestVerdictLabelExhaustedHeadIsNeedsHuman(t *testing.T) {
	set := verdictLabelTestSet()
	state := DispatchState{
		Pending: []PendingReview{{Repo: "org/repo", Number: 8, HeadSHA: "held8", Perspective: PerspectiveCorrectness, Agent: "r"}},
		Human: []HumanReviewHold{{Repo: "org/repo", Number: 8, HeadSHA: "held8",
			Reason: HoldReasonDispatchExhausted + ": 3 review dispatches for this head ended without a verdict"}},
	}
	prs := []PullRequest{{Repo: "org/repo", Number: 8, HeadSHA: "held8", Labels: []string{"hive-review/in-progress"}}}
	got := PlanVerdictLabels(set, prs, Artifact{}, state, 0)
	if len(got) != 1 || got[0].Add != set.RequiresHuman || len(got[0].Remove) != 1 || got[0].Remove[0] != set.Reviewing {
		t.Fatalf("label changes = %+v, want add %q and remove %q", got, set.RequiresHuman, set.Reviewing)
	}
}

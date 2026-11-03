package review

import (
	"testing"
)

// The perspective cap is the hive's control over how much review traffic one
// pull request attracts. Each perspective is a separate review comment, so a
// cap that only applies within a cycle does not limit what a maintainer
// actually sees: the remaining perspectives simply arrive on later cycles.
//
// Measured on a bluefin spoke before this behaviour changed: projectbluefin/
// common#1011 collected six reviews in seventy-seven minutes, and
// fsdk-containers#225 was reviewed at 04:30 and again at 04:45 with no restart
// in between -- consecutive perspectives, one per cadence interval.

func cappedOpts(cap int) DispatchOptions {
	return DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		MaxParallelReviews:   10,
		MaxPerspectivesPerPR: cap,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")},
	}
}

// dispatchAllKicks runs successive cycles, carrying the pending state forward
// the way the eval loop does, and returns every review kick issued for the PR.
func dispatchAllKicks(t *testing.T, cap int, cycles int) []DispatchKick {
	t.Helper()
	pr := dispatchPRNum(1, "sha1")
	state := DispatchState{}
	var all []DispatchKick
	for i := 0; i < cycles; i++ {
		plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, cappedOpts(cap))
		all = append(all, plan.ReviewKicks...)
		// The eval loop confirms delivery, which is what leaves the dispatched
		// perspectives recorded as pending for the next cycle.
		state = ConfirmDelivered(plan.State, plan.ReviewKicks, plan.ReviewKicks)
	}
	return all
}

func TestPerspectiveCapIsALifetimeBudgetPerHeadSHA(t *testing.T) {
	// A cap of one means one review comment on that PR, not one per cycle
	// until every perspective has been used.
	kicks := dispatchAllKicks(t, 1, len(DefaultPerspectives)+2)
	if len(kicks) != 1 {
		t.Fatalf("cap of 1 produced %d review kicks across cycles, want 1", len(kicks))
	}
}

func TestPerspectiveCapAllowsExactlyItsBudget(t *testing.T) {
	kicks := dispatchAllKicks(t, 2, len(DefaultPerspectives)+2)
	if len(kicks) != 2 {
		t.Fatalf("cap of 2 produced %d review kicks across cycles, want 2", len(kicks))
	}
	seen := map[Perspective]bool{}
	for _, k := range kicks {
		if seen[k.Perspective] {
			t.Fatalf("perspective %q dispatched twice for the same head SHA", k.Perspective)
		}
		seen[k.Perspective] = true
	}
}

func TestUncappedStillReachesEveryPerspective(t *testing.T) {
	// The default must not change shape: a hive that wants depth keeps it.
	// One review kick is in flight per agent, so depth needs as many free
	// reviewers as perspectives (nothing answers in this simulation, so a
	// reviewer stays busy once kicked).
	pr := dispatchPRNum(1, "sha1")
	opts := cappedOpts(0)
	opts.Agents = []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3"), reviewer("r4"), reviewer("r5")}
	state := DispatchState{}
	var kicks []DispatchKick
	for i := 0; i < len(DefaultPerspectives)+2; i++ {
		plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, opts)
		kicks = append(kicks, plan.ReviewKicks...)
		state = ConfirmDelivered(plan.State, plan.ReviewKicks, plan.ReviewKicks)
	}
	if len(kicks) != len(DefaultPerspectives) {
		t.Fatalf("uncapped dispatch produced %d kicks, want %d (one per perspective)", len(kicks), len(DefaultPerspectives))
	}
}

func TestUncappedNeverStacksKicksOnABusyReviewer(t *testing.T) {
	// Three reviewers, five perspectives, and no verdicts: each reviewer takes
	// one perspective and the other two wait instead of being typed into a
	// session that is still reviewing.
	kicks := dispatchAllKicks(t, 0, len(DefaultPerspectives)+2)
	perAgent := map[string]int{}
	for _, k := range kicks {
		perAgent[k.Agent]++
	}
	if len(kicks) != 3 || perAgent["r1"] != 1 || perAgent["r2"] != 1 || perAgent["r3"] != 1 {
		t.Fatalf("kicks by agent = %v (%d kicks), want one each while nobody has answered", perAgent, len(kicks))
	}
}

func TestPerspectiveCapResetsOnForcePush(t *testing.T) {
	// New code deserves a fresh review budget. Pending entries are keyed by
	// head SHA and pruned when it moves, so the cap refills.
	state := DispatchState{}
	before := PlanDispatch([]PullRequest{dispatchPRNum(1, "sha-old")}, Artifact{}, state, cappedOpts(1))
	if len(before.ReviewKicks) != 1 {
		t.Fatalf("first cycle got %d kicks, want 1", len(before.ReviewKicks))
	}
	state = ConfirmDelivered(before.State, before.ReviewKicks, before.ReviewKicks)

	// Same PR, exhausted budget: nothing more.
	same := PlanDispatch([]PullRequest{dispatchPRNum(1, "sha-old")}, Artifact{}, state, cappedOpts(1))
	if len(same.ReviewKicks) != 0 {
		t.Fatalf("exhausted budget still dispatched %d kicks", len(same.ReviewKicks))
	}

	// Force-push: new head SHA, fresh budget.
	after := PlanDispatch([]PullRequest{dispatchPRNum(1, "sha-new")}, Artifact{}, state, cappedOpts(1))
	if len(after.ReviewKicks) != 1 {
		t.Fatalf("force-push should refill the budget, got %d kicks", len(after.ReviewKicks))
	}
	if after.ReviewKicks[0].HeadSHA != "sha-new" {
		t.Fatalf("kick targeted %q, want sha-new", after.ReviewKicks[0].HeadSHA)
	}
}

func TestPerspectiveCapStillSpreadsAcrossQueue(t *testing.T) {
	// Breadth-first behaviour is preserved: the cap must keep serving the
	// whole queue, not just limit the first PR.
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2"), dispatchPRNum(3, "sha3")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, cappedOpts(1))
	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("got %d kicks, want one per PR", len(plan.ReviewKicks))
	}
	seen := map[int]bool{}
	for _, k := range plan.ReviewKicks {
		seen[k.Number] = true
	}
	if len(seen) != 3 {
		t.Fatalf("cap should spread across the queue, covered %+v", seen)
	}
}

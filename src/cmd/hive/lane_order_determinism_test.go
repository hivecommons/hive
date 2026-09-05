package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// #5856, second defect: the classifier's lane table was built by ranging over
// cfg.Agents — a Go MAP — and classifyLane is first-match-wins over the
// resulting slice. Go randomizes map iteration order per range, so an issue
// matching two lanes went to whichever lane happened to come out of the map
// first, and the winner could differ between two runs of the same binary on the
// same config. Nothing recorded which one it had been.
//
// This is not hypothetical on a real L4 hive: "[ACMM L4] Add AI fix workflow"
// matches ci-maintainer's `workflow` and scanner's `fix`, and those two agents
// have different modes (ISSUES_AND_PRS vs ISSUES_ONLY), so the coin flip
// decided whether the issue could be fixed at all.

// TestInitAgentConfigDrivenSystemsSortsLanes asserts the lane table reaches the
// classifier in a deterministic order.
//
// It is a BEHAVIOURAL test, because activeLanes() is unexported: it classifies
// an issue that every configured lane matches, so the answer IS the first
// element of the table. Rebuilding the table repeatedly re-ranges the map each
// time, which is what makes the check meaningful — with six colliding lanes and
// no sort, an unsorted build lands on the alphabetically-first lane with
// probability 1/6 per iteration, so passing all 40 iterations by luck has
// probability (1/6)^40.
func TestInitAgentConfigDrivenSystemsSortsLanes(t *testing.T) {
	// Restore the classifier's package state for anything that runs after.
	t.Cleanup(func() { classify.SetLanes(nil) })

	// Every lane claims the same keyword, so every lane matches the issue below
	// and only the ORDER can decide. Names are chosen so the winner is not the
	// one a naive implementation would land on: "aardvark" is alphabetically
	// first but is neither the first nor the last literal here.
	const shared = "collide"
	agents := map[string]config.AgentConfig{
		"scanner":       {LaneKeywords: []string{shared}},
		"zebra":         {LaneKeywords: []string{shared}},
		"aardvark":      {LaneKeywords: []string{shared}},
		"ci-maintainer": {LaneKeywords: []string{shared}},
		"quality":       {LaneKeywords: []string{shared}},
		"sec-check":     {LaneKeywords: []string{shared}},
		// An agent with no lane keywords must stay out of the table entirely —
		// exactly the state `quality` is in on the real level-4 pack.
		"supervisor": {},
	}
	cfg := &config.Config{Agents: agents}
	issue := github.Issue{Title: "please collide these lanes"}

	const want = "aardvark"
	for i := 0; i < 40; i++ {
		initAgentConfigDrivenSystems(cfg)
		if got := classify.Classify(issue).Lane; string(got) != want {
			t.Fatalf("iteration %d: lane = %q, want %q — the lane table is not sorted, so a two-lane match resolves by Go's randomized map order", i, got, want)
		}
	}
}

// TestInitAgentConfigDrivenSystemsKeywordlessAgentsStayOutOfTheTable pins the
// other half of the build: an agent that declares no lane_keywords is not a
// lane. It matters because a keywordless agent silently in the table would
// capture the DefaultLane fallback ordering, and because it is the real state
// of `quality` at L4 — which is a second, independent reason the reported ACMM
// issues never reached it.
func TestInitAgentConfigDrivenSystemsKeywordlessAgentsStayOutOfTheTable(t *testing.T) {
	t.Cleanup(func() { classify.SetLanes(nil) })

	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"aardvark": {},                                 // no keywords at all
		"scanner":  {LaneKeywords: []string{"triage"}}, // the only real lane
	}}
	initAgentConfigDrivenSystems(cfg)

	// "aardvark" sorts first, so if a keywordless agent were admitted to the
	// table it would win this by label-name routing.
	got := classify.Classify(github.Issue{Title: "needs triage", Labels: []string{"aardvark"}}).Lane
	if string(got) == "aardvark" {
		t.Error("an agent with no lane_keywords was admitted to the lane table")
	}
	if string(got) != "scanner" {
		t.Errorf("lane = %q, want scanner", got)
	}
}

package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// hivecommons/hive#15499: the reviewer asked "close #15499 as a no-op?"
// and named #15427 as the PR carrying the change, but the tracker showed
// "a reviewer handed this PR to a person without a specific question"
// because the current head (a merge-from-base) had no verdict yet.
func TestStageTrackerDecisionVerdictLookupFallsBackToNewestDecisionOnAnyHead(t *testing.T) {
	older := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	art := &review.Artifact{Items: []review.Aggregate{
		{Repo: "hivecommons/hive", Number: 15499, HeadSHA: "c2fbc9d", Verdict: review.VerdictRequiresHuman, RequiresHuman: true, DecisionRequested: true, DecisionQuestion: "close as a no-op? #15427 landed it", RedundantOf: 15427, RecordedAt: older},
		{Repo: "hivecommons/hive", Number: 15499, HeadSHA: "eca280c", Verdict: review.VerdictRequiresHuman, RequiresHuman: true, DecisionRequested: true, DecisionQuestion: "the tree matches v5 via #15427 — close as superseded?", RedundantOf: 15427, RecordedAt: older.Add(time.Hour)},
		{Repo: "hivecommons/hive", Number: 15499, HeadSHA: "e794b31", Verdict: review.VerdictApprove, RecordedAt: older.Add(2 * time.Hour)},
	}}
	lookup := stageTrackerDecisionVerdictLookup(art)
	if a, ok := lookup("hivecommons/hive", 15499, "e794b31", true); !ok || a.HeadSHA != "e794b31" {
		t.Fatalf("exact head must win: %+v %v", a, ok)
	}
	a, ok := lookup("hivecommons/hive", 15499, "bdb3d86", true)
	if !ok || a.HeadSHA != "eca280c" || a.RedundantOf != 15427 {
		t.Fatalf("parked row with no verdict at head: got %+v %v, want newest decision-requested", a, ok)
	}
	if _, ok := lookup("hivecommons/hive", 15499, "bdb3d86", false); ok {
		t.Fatal("a row not parked on a decision does not borrow another head's verdict")
	}
}

func TestDecisionForPRRedundantOfLeadsWithCloseSuperseded(t *testing.T) {
	it := StageTrackerItem{Repo: "hivecommons/hive", Number: 15499, Kind: "pr", Stage: 85, Head: "bdb3d86", Reason: "verdict:decision-requested"}
	v := &review.Aggregate{Verdict: review.VerdictRequiresHuman, DecisionRequested: true, DecisionQuestion: "@clubanderson close #15499 as a no-op? #15427 already carries the fix.", RedundantOf: 15427, RecordedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
	d := decisionForPR(it, v, "", false)
	if d.Question != v.DecisionQuestion || d.AskedBy != decisionAskedByReviewer || d.RedundantOf != 15427 {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Options) == 0 || d.Options[0].Action != decisionActionCloseSuperseded || !strings.Contains(d.Options[0].Consequence, "#15427") {
		t.Fatalf("lead option = %+v, want close-superseded naming #15427", d.Options)
	}
	seen := 0
	for _, o := range d.Options {
		if o.Action == decisionActionCloseSuperseded {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("close-superseded listed %d times", seen)
	}
	// Redundant-of without a quoted question still says what was found.
	v.DecisionQuestion = ""
	d = decisionForPR(it, v, "", false)
	if !strings.Contains(d.Question, "#15427") || !strings.Contains(d.Question, "superseded") {
		t.Fatalf("question = %q", d.Question)
	}
	// An ordinary decision keeps the standard order.
	plain := decisionForPR(it, &review.Aggregate{DecisionRequested: true, DecisionQuestion: "rename the flag?"}, "", false)
	if plain.RedundantOf != 0 || plain.Options[0].Action != decisionActionDelegate {
		t.Fatalf("plain decision = %+v", plain)
	}
}

package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15744: a hive PR the fix dispatcher refuses must not
// read "Fixing · @bot" at 070, and at 085 its lead answer is Re-author.

func TestStageTrackerApplyFixerStatusExhausted(t *testing.T) {
	exhausted := review.FixDispatchStatus{Reason: review.FixDispatchExcludedAttemptsExhausted, Attempts: 6}
	base := func() StageTrackerItem {
		return StageTrackerItem{
			Repo: "hivecommons/hive", Number: 15308, Kind: "pr", Stage: trackerNumberFixing,
			Caption:    "Fixing",
			Modifiers:  []string{modNeedsRebase},
			StageActor: &StageTrackerActor{Kind: StageActorHive, Login: "hivecommons-hive[bot]"},
		}
	}

	it := base()
	stageTrackerApplyFixerStatus(&it, exhausted, nil)
	if !strings.HasPrefix(it.Caption, "Fixing paused — fixer gave up after 6 attempts") || !strings.Contains(it.Caption, "conflicting with the base branch") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.StageActor.Kind != StageActorNone || it.StageActor.Login != "" || it.Actor != "maintainer" {
		t.Fatalf("actor = %+v (row actor %q)", it.StageActor, it.Actor)
	}

	// A worker on the PR means a fix really is running: leave the row alone.
	busy := base()
	workers := map[string]stageTrackerWorker{"hivecommons/hive#15308": {}}
	stageTrackerApplyFixerStatus(&busy, exhausted, workers)
	if busy.Caption != "Fixing" || busy.StageActor.Kind != StageActorHive {
		t.Fatalf("busy row changed: %q %+v", busy.Caption, busy.StageActor)
	}

	// Active / not-excluded dispatch: unchanged.
	active := base()
	stageTrackerApplyFixerStatus(&active, review.FixDispatchStatus{Active: true, Agent: "claude"}, nil)
	if active.Caption != "Fixing" || active.StageActor.Kind != StageActorHive {
		t.Fatalf("active row changed: %q %+v", active.Caption, active.StageActor)
	}

	// Off the fixing stage: unchanged.
	parked := base()
	parked.Stage = trackerNumberNeedsDecision
	stageTrackerApplyFixerStatus(&parked, exhausted, nil)
	if parked.Caption != "Fixing" {
		t.Fatalf("085 row changed: %q", parked.Caption)
	}
}

func TestStageTrackerFixerExhaustedCaption(t *testing.T) {
	if got := stageTrackerFixerExhaustedCaption(6, "tests fail on arm64\nsecond line"); got != "fixer gave up after 6 attempts — tests fail on arm64" {
		t.Fatalf("caption = %q", got)
	}
	if got := stageTrackerFixerExhaustedCaption(3, ""); got != "fixer gave up after 3 attempts" {
		t.Fatalf("caption = %q", got)
	}
}

func TestDecisionForPRFixerExhaustedLeadsWithReauthor(t *testing.T) {
	it := StageTrackerItem{
		Repo: "hivecommons/hive", Number: 15308, Kind: "pr", Stage: trackerNumberNeedsDecision,
		AuthorKind: authorKindHive,
		Reason:     stagelabels.FixerExhaustedReason(6),
	}
	d := decisionForPR(it, &review.Aggregate{HumanSummaries: []string{"tests fail on arm64"}}, "", false)
	if d == nil || !strings.Contains(d.Question, "6 attempts") || !strings.Contains(d.Question, "tests fail on arm64") {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Options) == 0 || d.Options[0].Action != decisionActionReauthor || !d.Options[0].Lead || !d.Options[0].Enabled {
		t.Fatalf("lead option = %+v", d.Options)
	}
	var sawClose bool
	for i, o := range d.Options {
		if o.Action == decisionActionCloseWontfix {
			sawClose = true
			if i == 0 || o.Lead || o.Label != "Close as not planned" {
				t.Fatalf("close option = %+v at %d", o, i)
			}
		}
		if o.Action == decisionActionReauthor && i != 0 {
			t.Fatalf("duplicate re-author option at %d: %+v", i, d.Options)
		}
	}
	if !sawClose {
		t.Fatalf("no close-as-not-planned option: %+v", d.Options)
	}
}

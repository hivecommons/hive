package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15744: the hive's own PR whose fix dispatcher gave up
// derives 085 with the attempt count in the reason; a stale answered
// decision on an unmoved head must not pull it back to 070.
func hiveFixingFacts(reason string) Facts {
	return Facts{
		Ref:    Ref{Owner: "o", Repo: "r", Number: 15308, Kind: stagelabels.PRItem},
		Open:   true,
		Labels: []string{"stage/070-fixing", "author/hive"},
		PR: &PRFacts{
			HeadSHA: goldenHead,
			Lanes:   []LaneFact{{Lane: stageLaneSelf, Reason: reason}},
		},
	}
}

func TestDerivePRFixerExhaustedParksAtNeedsDecision(t *testing.T) {
	f := hiveFixingFacts(contributorLaneReasonChangesRequested)
	if d := DerivePR(f); d.Stage != stagelabels.Fixing {
		t.Fatalf("baseline stage = %s (%q), want fixing", d.Stage, d.Reason)
	}
	f.PR.Pass = &PRPassFacts{AuthorIsApp: true, FixerExhausted: 6, FixerExcluded: true}
	d := DerivePR(f)
	if d.Stage != stagelabels.NeedsDecision || d.Reason != stagelabels.FixerExhaustedReason(6) {
		t.Fatalf("exhausted: stage = %s reason = %q, want 085 fixer-exhausted:6", d.Stage, d.Reason)
	}
	// Off the fixing stage the fact is inert.
	g := hiveFixingFacts("ci-pending")
	g.PR.Pass = &PRPassFacts{AuthorIsApp: true, FixerExhausted: 6, FixerExcluded: true}
	if d := DerivePR(g); d.Stage == stagelabels.NeedsDecision {
		t.Fatalf("exhausted fact moved a non-fixing PR to 085: %+v", d)
	}
}

func TestPreserveStageOnStaleDecisionAnswer(t *testing.T) {
	obs := stageObservation{Lane: stageLaneSelf, HeadSHA: goldenHead, AuthorKind: authorkind.Hive}
	fixing := stageVerdict{Stage: FixingStage(obs), Rank: stageRankOwned, Reason: StageReasonDecisionAnswered}
	prev := stagelabels.Record{Stage: stagelabels.NeedsDecision, Reason: stagelabels.FixerExhaustedReason(6), ActivityHead: goldenHead}

	got, ok := PreserveStageOnStaleDecisionAnswer(prev, true, obs, fixing, true)
	if !ok || got.Stage != stagelabels.NeedsDecision || got.Reason != prev.Reason {
		t.Fatalf("excluded fixer, same head: got %+v %v, want prior 085 kept", got, ok)
	}
	if _, ok := PreserveStageOnStaleDecisionAnswer(prev, true, obs, fixing, false); ok {
		t.Fatal("a fixer that can act must take the answered decision to 070")
	}
	moved := prev
	moved.ActivityHead = "0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0"
	if _, ok := PreserveStageOnStaleDecisionAnswer(moved, true, obs, fixing, true); ok {
		t.Fatal("a new head is a fresh decision; the guard must not hold it")
	}
	other := fixing
	other.Reason = autoMergeReasonHumanChangesRequested
	if _, ok := PreserveStageOnStaleDecisionAnswer(prev, true, obs, other, true); ok {
		t.Fatal("only the answered-decision reason is guarded")
	}
	if _, ok := PreserveStageOnStaleDecisionAnswer(stagelabels.Record{Stage: stagelabels.Fixing, ActivityHead: goldenHead}, true, obs, fixing, true); ok {
		t.Fatal("a PR not previously at 085 has nothing to preserve")
	}
}

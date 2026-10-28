package stageengine

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14679 (seen on #14628): a trusted human's recorded
// decision on the head answers the swarm's decision-requested verdict on
// that head. With an instruction the PR is fixing; a bare approval reads as
// a human approval and releases requires_human; a decision recorded for an
// older head is ignored and the question stands.
func TestClassifyStageHumanDecisionSupersedesDecisionRequested(t *testing.T) {
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
	fixing := FixingStage(stageObservation{Lane: stageLaneSelf})
	cases := []struct {
		name       string
		reason     string
		detail     stageDetail
		wantStage  stagelabels.Stage
		wantReason string
		wantHuman  bool
	}{
		{
			name:       "instructions on the head → fixing, over the lane's human-changes-requested reason",
			reason:     autoMergeReasonHumanChangesRequested,
			detail:     stageDetail{VerdictKnown: true, Verdict: asked, HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead},
			wantStage:  fixing,
			wantReason: autoMergeReasonHumanChangesRequested,
		},
		{
			name:       "instructions on the head → fixing, whatever else the lane reported",
			reason:     "ci-pending",
			detail:     stageDetail{VerdictKnown: true, Verdict: asked, HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead},
			wantStage:  fixing,
			wantReason: autoMergeReasonHumanChangesRequested,
		},
		{
			name:      "bare approve on the head → the merge path, not 085",
			reason:    "ci-pending",
			detail:    stageDetail{VerdictKnown: true, Verdict: asked, HumanApprover: "clubanderson", HumanApprovalHead: goldenHead},
			wantStage: stagelabels.None,
		},
		{
			name:       "decision recorded for an older head → the question on this head stands",
			reason:     "ci-pending",
			detail:     stageDetail{VerdictKnown: true, Verdict: asked, HumanChangeRequester: "clubanderson", HumanChangeRequestHead: "0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0"},
			wantStage:  stagelabels.NeedsDecision,
			wantReason: StageReasonDecisionRequested,
			wantHuman:  true,
		},
		{
			name:       "no decision → 085 as before",
			reason:     "ci-pending",
			detail:     stageDetail{VerdictKnown: true, Verdict: asked},
			wantStage:  stagelabels.NeedsDecision,
			wantReason: StageReasonDecisionRequested,
			wantHuman:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyStage(stageObservation{Lane: stageLaneSelf, Reason: tc.reason, HeadSHA: goldenHead, Detail: tc.detail})
			if v.Stage == stagelabels.NeedsDecision && tc.wantStage != stagelabels.NeedsDecision {
				t.Fatalf("classified as %+v, want not needs-decision", v)
			}
			if tc.wantStage != stagelabels.None && (v.Stage != tc.wantStage || v.Reason != tc.wantReason || v.Human != tc.wantHuman) {
				t.Fatalf("classified as %+v, want stage=%s reason=%q human=%v", v, tc.wantStage, tc.wantReason, tc.wantHuman)
			}
		})
	}
}

// The admission branch (a lane that cannot vouch for the author) takes the
// same answer: the owner's instruction on the head is fixing, not 085.
func TestClassifyStageHumanDecisionSupersedesDecisionRequestedOnAdmission(t *testing.T) {
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
	for reason, rank := range stageAdmissionReasons {
		if rank == stageRankNone {
			continue
		}
		o := stageObservation{Lane: stageLaneTrusted, Reason: reason, HeadSHA: goldenHead, Detail: stageDetail{VerdictKnown: true, Verdict: asked, HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead}}
		v := ClassifyStage(o)
		if v.Stage != FixingStage(o) || v.Reason != autoMergeReasonHumanChangesRequested || v.Rank != stageRankOwned {
			t.Fatalf("%q classified as %+v, want fixing on the owner's instruction", reason, v)
		}
		o.Detail.HumanChangeRequestHead = "0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0ld0"
		if v := ClassifyStage(o); v.Stage != stagelabels.NeedsDecision {
			t.Fatalf("%q with a stale decision classified as %+v, want needs-decision", reason, v)
		}
	}
}

// ReviewFacts round-trips the requester only for the matching head, so a
// decision carried over from an older head cannot un-park the new one.
func TestReviewFactsCarryHumanChangeRequesterForHeadOnly(t *testing.T) {
	d := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}, HumanChangeRequester: "clubanderson", HumanChangeRequestHead: goldenHead}
	if r := ReviewFactsFrom(d, goldenHead); r.HumanChangeRequester != "clubanderson" {
		t.Fatalf("ReviewFactsFrom lost the requester: %+v", r)
	}
	if r := ReviewFactsFrom(d, "other"); r.HumanChangeRequester != "" {
		t.Fatalf("ReviewFactsFrom carried the requester to another head: %+v", r)
	}
	f := Facts{Open: true, PR: &PRFacts{HeadSHA: goldenHead, Review: ReviewFactsFrom(d, goldenHead), Lanes: []LaneFact{{Lane: stageLaneSelf, Reason: "ci-pending"}}}}
	obs := ObservationsFromPRFacts(f)
	if len(obs) != 1 || obs[0].Detail.HumanChangeRequester != "clubanderson" || obs[0].Detail.HumanChangeRequestHead != goldenHead {
		t.Fatalf("observations = %+v", obs)
	}
	if v := ClassifyStage(obs[0]); v.Stage != FixingStage(obs[0]) {
		t.Fatalf("round-tripped observation classified as %+v, want fixing", v)
	}
}

// PRDecided derives the stage the reply promises: an instruction on a
// hive-authored PR is 070 now; a bare approval abstains and leaves the
// merge path to the lanes' next pass; a decision with no head abstains.
func TestFactSinkPRDecidedDerivesFixingForInstruction(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 14628, Kind: stagelabels.PRItem}
	cases := []struct {
		name string
		fact PRDecisionFact
		want stagelabels.Stage
	}{
		{"instruction on a hive PR → 070", PRDecisionFact{Head: goldenHead, By: "clubanderson", Instruction: true, SelfAuthored: true}, stagelabels.Fixing},
		{"instruction on a contributor PR → fixing for its author", PRDecisionFact{Head: goldenHead, By: "clubanderson", Instruction: true}, FixingStage(stageObservation{Lane: stageLaneTrusted})},
		{"bare approve → no opinion here", PRDecisionFact{Head: goldenHead, By: "clubanderson"}, stagelabels.None},
		{"no head → no opinion", PRDecisionFact{By: "clubanderson", Instruction: true, SelfAuthored: true}, stagelabels.None},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Facts
			sink := NewFactSink(deriveOnlyReconciler{on: func(f Facts) { got = f }}, FactSinkOptions{})
			if _, err := sink.PRDecided(context.Background(), ref, tc.fact); err != nil {
				t.Fatal(err)
			}
			d := derivePR(got)
			if tc.want == stagelabels.None {
				if !d.Abstain {
					t.Fatalf("derived %+v, want abstain", d)
				}
				return
			}
			if d.Abstain || d.Stage != tc.want || d.Reason != autoMergeReasonHumanChangesRequested {
				t.Fatalf("derived %+v, want %s", d, tc.want)
			}
		})
	}
}

// deriveOnlyReconciler hands the Facts a sink built to the test instead of
// writing anything.
type deriveOnlyReconciler struct{ on func(Facts) }

func (r deriveOnlyReconciler) Reconcile(_ context.Context, f Facts) (Transition, error) {
	r.on(f)
	return Transition{Ref: f.Ref}, nil
}

func (r deriveOnlyReconciler) ReconcileBatch(ctx context.Context, facts []Facts) ([]Transition, error) {
	out := make([]Transition, 0, len(facts))
	for _, f := range facts {
		tr, _ := r.Reconcile(ctx, f)
		out = append(out, tr)
	}
	return out, nil
}

// hivecommons/hive#14679 (the 070↔085 flap on #14628): the decision answer
// rides on the reviewer verdict for the head, so a reconcile tick that only
// sees the verdict — no lane noted a requester this cycle — still derives
// fixing with reason decision-answered, never 085. A new head's fresh
// requires_human verdict carries no answer and parks again; a decision
// recorded for the old head does not leak onto the new one.
func TestClassifyStageDecisionAnsweredVerdictNeverParks(t *testing.T) {
	const newHead = "beef5678newhead"
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
	answered := asked.WithDecisionAnswer("clubanderson", goldenHead, prstage.DecisionDirectiveChanges)
	approved := asked.WithDecisionAnswer("clubanderson", goldenHead, prstage.DecisionDirectiveApprove)
	fixing := FixingStage(stageObservation{Lane: stageLaneSelf})
	cases := []struct {
		name       string
		head       string
		reason     string
		verdict    ContributorLaneVerdict
		labels     []string
		wantStage  stagelabels.Stage
		wantReason string
	}{
		{name: "tick after the instruction → 070 decision-answered", head: goldenHead, reason: "ci-pending", verdict: answered, wantStage: fixing, wantReason: StageReasonDecisionAnswered},
		{name: "tick with the lane's own decision-requested reason → still 070", head: goldenHead, reason: StageReasonDecisionRequested, verdict: answered, wantStage: fixing, wantReason: StageReasonDecisionAnswered},
		{name: "manual 070 label stands while the decision stands", head: goldenHead, reason: "ci-pending", verdict: answered, labels: []string{stagelabels.Fixing.Label()}, wantStage: fixing, wantReason: StageReasonDecisionAnswered},
		{name: "bare approve → not parked", head: goldenHead, reason: "ci-pending", verdict: approved, wantStage: stagelabels.None},
		{name: "new head, fresh requires_human → 085 again", head: newHead, reason: "ci-pending", verdict: asked, wantStage: stagelabels.NeedsDecision, wantReason: StageReasonDecisionRequested},
		{name: "new head carrying the old head's answer → 085 again", head: newHead, reason: "ci-pending", verdict: answered, wantStage: stagelabels.NeedsDecision, wantReason: StageReasonDecisionRequested},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := stageObservation{Lane: stageLaneSelf, Reason: tc.reason, HeadSHA: tc.head, Labels: tc.labels, Detail: stageDetail{VerdictKnown: true, Verdict: tc.verdict}}
			v := ClassifyStage(o)
			if tc.wantStage == stagelabels.None {
				if v.Stage == stagelabels.NeedsDecision || v.Stage == fixing {
					t.Fatalf("classified as %+v, want the merge path", v)
				}
				return
			}
			if v.Stage != tc.wantStage || v.Reason != tc.wantReason {
				t.Fatalf("classified as %+v, want stage=%s reason=%q", v, tc.wantStage, tc.wantReason)
			}
			if tc.wantStage == fixing && v.Human {
				t.Fatalf("classified as %+v, want no human route", v)
			}
			// The derive path (startup reconcile, dashboard) reads the same
			// answer through ReviewFacts.
			facts := FactsFromPRObservations([]stageObservation{o})
			if facts.PR == nil {
				t.Fatal("no PR facts")
			}
			if got := ClassifyStage(ObservationsFromPRFacts(facts)[0]); got.Stage != v.Stage || got.Reason != v.Reason {
				t.Fatalf("derived %+v, want %+v", got, v)
			}
		})
	}
}

func TestReviewFactsRoundTripDecisionAnswer(t *testing.T) {
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
	d := stageDetail{VerdictKnown: true, Verdict: asked.WithDecisionAnswer("clubanderson", goldenHead, prstage.DecisionDirectiveChanges)}
	r := ReviewFactsFrom(d, goldenHead)
	if !r.DecisionAnswered || r.DecisionDirective != prstage.DecisionDirectiveChanges || r.HumanChangeRequester != "clubanderson" {
		t.Fatalf("ReviewFacts = %+v", r)
	}
	if r := ReviewFactsFrom(d, "0ther"); r.DecisionAnswered || r.HumanChangeRequester != "" {
		t.Fatalf("answer leaked onto another head: %+v", r)
	}
	d = stageDetail{VerdictKnown: true, Verdict: asked.WithDecisionAnswer("clubanderson", goldenHead, prstage.DecisionDirectiveApprove)}
	if r := ReviewFactsFrom(d, goldenHead); !r.DecisionAnswered || r.DecisionDirective != prstage.DecisionDirectiveApprove || r.HumanApprover != "clubanderson" || r.HumanChangeRequester != "" {
		t.Fatalf("approve ReviewFacts = %+v", r)
	}
}

package automerge

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14679 (the 070↔085 flap on #14628): the reconcile tick
// re-derived 085 from the persisted decision-requested verdict because no
// lane had noted the owner's answer that cycle. The pass now folds the
// durable decision record into every observation's verdict before
// classification, so the tick yields 070 decision-answered — whatever the
// lanes reported and however the PR reached 070 (planner, manual label) —
// and a new head with a fresh requires_human verdict parks again.
func TestFoldRecordedDecisionAnswersVerdictRoute(t *testing.T) {
	const head, newHead = "3b4a2b5510feb333579520508478acd0bc1b2ccc", "beefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, HiveAuthoredKnown: true, HiveAuthored: true}
	store := review.NewHumanChangeRequests("")
	if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 14628, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision, SourceID: 6450262272, Actor: "clubanderson", Body: "approve\n1. Change `Closes #14605` to `Refs #14605`."}); err != nil {
		t.Fatal(err)
	}
	c := &Engine{humanChangeRequests: store}

	for _, tc := range []struct {
		name       string
		head       string
		labels     []string
		wantStage  stagelabels.Stage
		wantReason string
	}{
		{name: "tick after the decision → 070 decision-answered", head: head, wantStage: stagelabels.Fixing, wantReason: stageReasonDecisionAnswered},
		{name: "manual 070 is not reverted", head: head, labels: []string{stagelabels.Fixing.Label()}, wantStage: stagelabels.Fixing, wantReason: stageReasonDecisionAnswered},
		{name: "new head, fresh requires_human → 085", head: newHead, wantStage: stagelabels.NeedsDecision, wantReason: stageReasonDecisionRequested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := []stageObservation{
				{Lane: stageLaneSelf, Owner: "hivecommons", Repo: "hive", Number: 14628, HeadSHA: tc.head, Labels: tc.labels, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: asked}},
				{Lane: stageLaneContributor, Owner: "hivecommons", Repo: "hive", Number: 14628, HeadSHA: tc.head, Labels: tc.labels, Reason: contributorLaneReasonAppAuthored, Detail: stageDetail{VerdictKnown: true, Verdict: asked}},
			}
			c.foldRecordedDecision(context.Background(), list)
			got, _, obs := resolveStage(list)
			if got.Stage != tc.wantStage || got.Reason != tc.wantReason {
				t.Fatalf("stage=%s reason=%q (lane %s), want %s %q", got.Stage, got.Reason, obs.Lane, tc.wantStage, tc.wantReason)
			}
			add, remove := stagelabels.Plan(tc.labels, stagelabels.Desired{Stage: got.Stage})
			if len(tc.labels) > 0 && (hasStageLabel(remove, stagelabels.Fixing.Label()) || hasStageLabel(add, stagelabels.NeedsDecision.Label())) {
				t.Fatalf("manual 070 would be rewritten: add=%v remove=%v", add, remove)
			}
		})
	}
}

// The lanes' own verdict lookup carries the same answer, so the review gate
// and the stage tracker never see an unanswered decision-requested verdict
// for a head the owner already answered; a bare approve reads as approve.
func TestContributorLaneVerdictForCarriesRecordedDecision(t *testing.T) {
	const head = "3b4a2b5510feb333579520508478acd0bc1b2ccc"
	asked := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
	for _, tc := range []struct {
		name, kind, wantDirective string
	}{
		{name: "instruction", kind: review.HumanChangeRequestKindChanges, wantDirective: review.DecisionDirectiveChanges},
		{name: "bare approve", kind: review.HumanChangeRequestKindApprove, wantDirective: review.DecisionDirectiveApprove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := review.NewHumanChangeRequests("")
			if err := store.Record(review.HumanChangeRequest{Repo: "hivecommons/hive", Number: 14628, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "ClubAnderson", Body: "approve", Kind: tc.kind}); err != nil {
				t.Fatal(err)
			}
			c := &Engine{humanChangeRequests: store, contributorLaneVerdict: func(string, int, string) ContributorLaneVerdict { return asked }}
			v := c.contributorLaneVerdictFor("hivecommons/hive", 14628, head)
			if !v.DecisionAnswered || v.DecisionAnsweredBy != "clubanderson" || v.DecisionAnsweredHead != head || v.DecisionDirective != tc.wantDirective {
				t.Fatalf("verdict = %+v", v)
			}
			if other := c.contributorLaneVerdictFor("hivecommons/hive", 14628, "0ther"); other.DecisionAnswered {
				t.Fatalf("answer leaked onto another head: %+v", other)
			}
			o := stageObservation{Lane: stageLaneSelf, HeadSHA: head, Reason: contributorLaneReasonNoVerdict, Detail: stageDetail{VerdictKnown: true, Verdict: v}}
			if got := classifyStage(o); got.Stage == stagelabels.NeedsDecision {
				t.Fatalf("classified as %+v, want the question answered", got)
			}
		})
	}
}

func hasStageLabel(list []string, want string) bool {
	for _, l := range list {
		if l == want {
			return true
		}
	}
	return false
}

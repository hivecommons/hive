package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/server#686 and projectbluefin/documentation#1709: GitHub
// held every pull_request run on the head for approval and the hive's policy
// did not approve them, while the activity monitor showed 060 "author
// association" and 050 "waiting for a verdict". Held runs park the PR at 085
// with the run ids, over whatever the lane said next.
func TestClassifyStageHeldWorkflowRunsParkForMaintainer(t *testing.T) {
	runs := []int64{44261912124, 44257752419}
	const want = "workflow-approval-required:held-runs:44261912124,44257752419"
	approve := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	cases := []struct {
		name  string
		obs   stageObservation
		want  stagelabels.Stage
		owned bool
	}{
		{name: "untrusted fork, author association", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonAuthorAssociation, AuthorKind: authorkind.Human, Detail: approve, HeldWorkflowRuns: runs}, want: stagelabels.NeedsDecision, owned: true},
		{name: "bot PR waiting for a verdict", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict, AuthorKind: authorkind.Unknown, HeldWorkflowRuns: runs}, want: stagelabels.NeedsDecision, owned: true},
		{name: "no linked issue", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoLinkedIssue, AuthorKind: authorkind.Unknown, HeldWorkflowRuns: runs}, want: stagelabels.NeedsDecision, owned: true},
		{name: "lane error", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "review-state-check", Err: true, HeldWorkflowRuns: runs}, want: stagelabels.NeedsDecision, owned: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyStage(tc.obs)
			if got.Stage != tc.want || got.Reason != want || got.Rank != stageRankOwned || !got.Human {
				t.Fatalf("ClassifyStage = %+v, want owned human %v with reason %q", got, tc.want, want)
			}
		})
	}

	// What the held runs do not override.
	for _, tc := range []struct {
		name string
		obs  stageObservation
	}{
		{name: "closed", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "closed", HeldWorkflowRuns: runs}},
		{name: "draft", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "draft", HeldWorkflowRuns: runs}},
		{name: "merged", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Merged: true, HeldWorkflowRuns: runs}},
		{name: "handed off", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: trustedBotLaneSkipReason, HeldWorkflowRuns: runs}},
		{name: "held", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "held", Labels: []string{"hold"}, HeldWorkflowRuns: runs}},
		{name: "the lane's own ok-to-test reason", obs: stageObservation{Lane: stageLaneTrusted, HeadSHA: "sha", Reason: "workflow-approval-required:ci.yml", HeldWorkflowRuns: runs}},
		{name: "the lane's own unauthorized reason", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: "unauthorized:approve-workflow-runs", HeldWorkflowRuns: runs}},
		{name: "no held runs", obs: stageObservation{Lane: stageLaneContributor, HeadSHA: "sha", Reason: contributorLaneReasonNoVerdict}},
	} {
		t.Run("keeps "+tc.name, func(t *testing.T) {
			if got := ClassifyStage(tc.obs); got.Reason == want {
				t.Fatalf("ClassifyStage = %+v, held runs must not decide this stage", got)
			}
		})
	}
}

// The held runs survive the Facts round trip, so DerivePR parks the PR
// exactly as the legacy pass does.
func TestHeldWorkflowRunsRoundTripFacts(t *testing.T) {
	obs := stageObservation{Lane: stageLaneContributor, Owner: "o", Repo: "r", Number: 686, HeadSHA: "sha", Reason: contributorLaneReasonAuthorAssociation, AuthorKind: authorkind.Human, HeldWorkflowRuns: []int64{7}}
	f := FactsFromPRObservations([]stageObservation{obs})
	if got := f.PR.HeldWorkflowRuns; len(got) != 1 || got[0] != 7 {
		t.Fatalf("PRFacts.HeldWorkflowRuns = %v", got)
	}
	back := ObservationsFromPRFacts(f)
	if len(back) != 1 {
		t.Fatalf("observations = %+v", back)
	}
	if v := ClassifyStage(back[0]); v.Stage != stagelabels.NeedsDecision || v.Reason != "workflow-approval-required:held-runs:7" {
		t.Fatalf("ClassifyStage after round trip = %+v", v)
	}
}

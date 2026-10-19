package github

import (
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestIssueDeriverReproducesIssueStageFor asserts the stageengine adapter
// derives exactly what IssueStageFor decides for every input shape the
// legacy test exercises, with the covering PR's stage carried in Facts.
func TestIssueDeriverReproducesIssueStageFor(t *testing.T) {
	prStage := func(repo string, number int) stagelabels.Stage {
		if repo == "acme/widgets" && number == 99 {
			return stagelabels.MergeQueue
		}
		return stagelabels.None
	}
	cases := []struct {
		name string
		in   IssueStageInput
	}{
		{"actionable", IssueStageInput{}},
		{"live human claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: "human", ClaimedBy: "bob"}},
		{"live agent claim", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent)}},
		{"in-progress sweep", IssueStageInput{Bucket: inProgressWorkBucket}},
		{"covered, PR not yet staged", IssueStageInput{Repo: "acme/widgets", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 5}}},
		{"covered, PR staged", IssueStageInput{Repo: "acme/widgets", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99}}},
		{"covered, cross-repo PR staged", IssueStageInput{Repo: "acme/other", Bucket: workBucketClaimedByPR, Claim: &IssueClaimContext{PRNumber: 99, PRRepo: "acme/widgets"}}},
		{"likely-done", IssueStageInput{Labels: []string{LikelyDoneLabel}}},
		{"filtered out", IssueStageInput{Bucket: "filtered"}},
		{"needs-human parked", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"needs-human", stagelabels.NeedsDecision.Label()}}},
		{"needs-direction parked", IssueStageInput{Bucket: "filtered", Reason: "needs-direction", Labels: []string{"help wanted", "needs-direction"}}},
		{"renamed needs-human bucket", IssueStageInput{Bucket: "filtered", Reason: "needs-human", Labels: []string{"triage/needs-maintainer"}}},
		{"parked label on claimed issue", IssueStageInput{Bucket: workBucketClaimed, ClaimKind: string(claims.KindAgent), Labels: []string{"needs-spec"}}},
		{"exempt label", IssueStageInput{Bucket: "filtered", Reason: "Exempt label", Labels: []string{"wontfix"}}},
		{"reporter triage", IssueStageInput{Bucket: "reporter_triage"}},
	}
	deriver := IssueDeriver()
	if deriver.Kind() != stagelabels.IssueItem {
		t.Fatal("kind")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantStage, _, wantReason := IssueStageFor(tc.in, prStage)
			f := issueFactsFrom(tc.in, prStage)
			if f.Ref.Kind != stagelabels.IssueItem || f.Issue == nil {
				t.Fatalf("facts %+v", f)
			}
			got := deriver.Derive(f)
			if got.Abstain || got.Stage != wantStage || got.Reason != wantReason {
				t.Fatalf("got %+v; want stage=%v reason=%q", got, wantStage, wantReason)
			}
			if got.Terminal != (wantStage == stagelabels.None && f.Issue.LikelyDone) {
				t.Fatalf("terminal %v for %+v", got.Terminal, f.Issue)
			}
			// Round trip: the input rebuilt from Facts decides the same.
			back := issueInputFrom(f)
			s2, _, r2 := IssueStageFor(back, prStage)
			if s2 != wantStage || r2 != wantReason {
				t.Fatalf("round trip %v/%q, want %v/%q (back=%+v)", s2, r2, wantStage, wantReason, back)
			}
		})
	}
}

func TestIssueDeriverAbstainsWithoutEvidence(t *testing.T) {
	d := IssueDeriver()
	if got := d.Derive(stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.PRItem}}); !got.Abstain {
		t.Fatalf("pr facts: %+v", got)
	}
	if got := d.Derive(stageengine.Facts{Ref: stageengine.Ref{Kind: stagelabels.IssueItem}}); !got.Abstain {
		t.Fatalf("nil issue: %+v", got)
	}
}

package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func derivedIssueFacts(mut func(f *Facts)) Facts {
	f := Facts{
		Ref:   Ref{Owner: "acme", Repo: "widgets", Number: 7, Kind: stagelabels.IssueItem},
		Open:  true,
		Issue: &IssueFacts{Bucket: BucketActionable, NeedsHumanLabel: "needs-human"},
	}
	if mut != nil {
		mut(&f)
	}
	return f
}

// TestDeriveIssueTable pins the bucket → stage mapping the engine took over
// from pkg/github.IssueStageFor (plan PR-F); the full corpus replays through
// the shim in pkg/github's issue_stage_goldens_test.
func TestDeriveIssueTable(t *testing.T) {
	pr99 := &Ref{Owner: "acme", Repo: "widgets", Number: 99, Kind: stagelabels.PRItem}
	cases := []struct {
		name    string
		mut     func(f *Facts)
		stage   stagelabels.Stage
		covered bool
		reason  string
	}{
		{"actionable", nil, stagelabels.Actionable, false, "actionable"},
		{"reporter triage bucket", func(f *Facts) {
			f.Issue.Bucket = BucketReporterTriage
			f.Issue.TriageAwaiting = "reporter-trust gate: awaiting the triage label"
		}, stagelabels.TriageNeeded, false, "triage needed: reporter-trust gate: awaiting the triage label"},
		{"awaiting label", func(f *Facts) { f.Issue.TriageAwaiting = "carries needs-triage" }, stagelabels.TriageNeeded, false, "triage needed: carries needs-triage"},
		{"live human claim", func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Holder: "bob", Kind: "human"}
		}, stagelabels.Claimed, false, "live issue claim"},
		{"live agent claim", func(f *Facts) { f.Issue.Bucket = BucketClaimed; f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent} }, stagelabels.Claimed, false, "live agent claim (no PR yet)"},
		{"claim with PR under way", func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent, PRStarted: true}
		}, stagelabels.PRInProgress, false, "live claim with a PR under way"},
		{"in-progress mark", func(f *Facts) { f.Issue.Bucket = BucketInProgress }, stagelabels.Claimed, false, "in-progress start mark (no PR yet)"},
		{"in-progress with PR", func(f *Facts) { f.Issue.Bucket = BucketInProgress; f.Issue.Claim = &ClaimFact{PRStarted: true} }, stagelabels.PRInProgress, false, "in-progress start mark with a PR under way"},
		{"covered, PR not staged", func(f *Facts) { f.Issue.Bucket = BucketCoveredByPR; f.Issue.CoveringPR = pr99 }, stagelabels.CoveredByPR, false, "open PR covers it"},
		{"covered, PR below 040", func(f *Facts) {
			f.Issue.Bucket = BucketCoveredByPR
			f.Issue.CoveringPR = pr99
			f.Issue.CoveringPRStage = stagelabels.Claimed
		}, stagelabels.CoveredByPR, false, "open PR covers it"},
		{"covered, PR staged", func(f *Facts) {
			f.Issue.Bucket = BucketCoveredByPR
			f.Issue.CoveringPR = pr99
			f.Issue.CoveringPRStage = stagelabels.MergeQueue
		}, stagelabels.CoveredByPR, true, "open PR #99 covers it (PR at 100-merge-queue)"},
		{"likely done", func(f *Facts) { f.Issue.LikelyDone = true }, stagelabels.None, false, "merged PR awaiting confirmation"},
		{"filtered", func(f *Facts) { f.Issue.Bucket = BucketFiltered; f.Issue.FilteredReason = "Exempt label" }, stagelabels.None, false, "outside the pipeline: filtered"},
		{"needs-human", func(f *Facts) { f.Issue.Bucket = BucketFiltered; f.Park.NeedsHuman = true }, stagelabels.IssueNeedsDecision, false, "held for a person: needs-human"},
		{"parked on a person", func(f *Facts) { f.Issue.Bucket = BucketFiltered; f.Park.DecisionLabel = "needs-direction" }, stagelabels.IssueNeedsDecision, false, "parked on a person: needs-direction"},
		{"park outranks claim", func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent}
			f.Park.DecisionLabel = "needs-spec"
		}, stagelabels.IssueNeedsDecision, false, "parked on a person: needs-spec"},
		{"hold outranks park", func(f *Facts) { f.Issue.HoldLabel = "hold"; f.Park.NeedsHuman = true }, stagelabels.None, false, "held: hold"},
		{"blocked", func(f *Facts) {
			f.Issue.BlockedReason = "blocked on acme/widgets#7"
			f.Park.DecisionLabel = "needs-spec"
		}, stagelabels.None, false, "blocked on acme/widgets#7"},
		{"umbrella claim not staged", func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent}
			f.Issue.UmbrellaLabel = "epic"
		}, stagelabels.Actionable, false, "umbrella issue (epic): claim not staged"},
		{"umbrella untriaged", func(f *Facts) {
			f.Issue.Bucket = BucketInProgress
			f.Issue.UmbrellaLabel = "epic"
			f.Issue.TriageAwaiting = "carries needs-triage"
		}, stagelabels.TriageNeeded, false, "triage needed: carries needs-triage"},
		{"umbrella actionable is plain 010", func(f *Facts) { f.Issue.UmbrellaLabel = "epic" }, stagelabels.Actionable, false, "actionable"},
		{"unknown bucket", func(f *Facts) { f.Issue.Bucket = "weird" }, stagelabels.None, false, "outside the pipeline: weird"},
		{"decomposed parent", func(f *Facts) { f.Issue.OpenChildren = 2 }, stagelabels.Actionable, false, "actionable; waiting on 2 open child issue(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveIssue(derivedIssueFacts(tc.mut))
			if d.Abstain || d.Stage != tc.stage || d.Covered != tc.covered || d.Reason != tc.reason {
				t.Fatalf("got %+v; want %v covered=%v %q", d, tc.stage, tc.covered, tc.reason)
			}
		})
	}
}

func TestDeriveIssueFlags(t *testing.T) {
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Open = false; f.Issue.Bucket = BucketClaimed })); d.Stage != stagelabels.None || !d.Terminal || d.Reason != reasonIssueClosed {
		t.Fatalf("closed: %+v", d)
	}
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Issue.LikelyDone = true })); !d.Terminal {
		t.Fatalf("likely-done is terminal: %+v", d)
	}
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Issue.HoldLabel = "hold" })); d.Terminal {
		t.Fatalf("held is a pause, not terminal: %+v", d)
	}
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Issue.Stalled = true })); !d.Stalled {
		t.Fatalf("stalled carried: %+v", d)
	}
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Issue.Stalled = true; f.Issue.HoldLabel = "hold" })); d.Stalled {
		t.Fatalf("stalled dropped with the stage: %+v", d)
	}
	if d := DeriveIssue(derivedIssueFacts(func(f *Facts) { f.Issue = nil })); !d.Abstain || d.Reason != reasonIssueNoBucket {
		t.Fatalf("no bucket: %+v", d)
	}
	if d := DeriveIssue(Facts{Ref: Ref{Kind: stagelabels.PRItem}}); !d.Abstain {
		t.Fatalf("pr facts: %+v", d)
	}
}

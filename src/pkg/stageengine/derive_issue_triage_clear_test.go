package stageengine

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A claimed (020), PR-in-progress (030) or covered (040) issue still wearing
// the reporter-trust awaiting label asks the writer to clear it in the same
// label write (hivecommons/hive#14586): 015 is not a gate, so the label is
// stale once the hive has moved on. Before the claim — 010, 015, the 045
// park — and when the label is absent, nothing is cleared.
func TestDeriveIssueClearsStaleTriageLabelPastClaim(t *testing.T) {
	const label = "needs-triage"
	wearing := func(mut func(f *Facts)) func(f *Facts) {
		return func(f *Facts) {
			f.Labels = append(f.Labels, label)
			f.Issue.TriageAwaiting = "carries " + label
			f.Issue.TriageAwaitingLabel = label
			if mut != nil {
				mut(f)
			}
		}
	}
	cases := []struct {
		name  string
		mut   func(f *Facts)
		stage stagelabels.Stage
		clear []string
	}{
		{"015 keeps the label", wearing(nil), stagelabels.TriageNeeded, nil},
		{"direct claim 015→020 clears it", wearing(func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Holder: "scanner", Kind: claims.KindAgent}
		}), stagelabels.Claimed, []string{label}},
		{"in-progress mark clears it", wearing(func(f *Facts) { f.Issue.Bucket = BucketInProgress }), stagelabels.Claimed, []string{label}},
		{"PR under way clears it", wearing(func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent, PRStarted: true}
		}), stagelabels.PRInProgress, []string{label}},
		{"covered by PR clears it", wearing(func(f *Facts) {
			f.Issue.Bucket = BucketCoveredByPR
			f.Issue.CoveringPR = &Ref{Owner: "acme", Repo: "widgets", Number: 99, Kind: stagelabels.PRItem}
		}), stagelabels.CoveredByPR, []string{label}},
		{"045 park keeps the label", wearing(func(f *Facts) { f.Park.DecisionLabel = "needs-spec" }), stagelabels.IssueNeedsDecision, nil},
		{"umbrella claim stays untriaged", wearing(func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent}
			f.Issue.UmbrellaLabel = "epic"
		}), stagelabels.TriageNeeded, nil},
		{"claimed without the label is a no-op", func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent}
		}, stagelabels.Claimed, nil},
		{"closed clears nothing extra", wearing(func(f *Facts) { f.Open = false }), stagelabels.None, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveIssue(derivedIssueFacts(tc.mut))
			if d.Stage != tc.stage {
				t.Fatalf("stage = %s, want %s (%+v)", d.Stage, tc.stage, d)
			}
			if !slices.Equal(d.Clear, tc.clear) {
				t.Fatalf("Clear = %v, want %v", d.Clear, tc.clear)
			}
			if got := d.Desired().Clear; !slices.Equal(got, tc.clear) {
				t.Fatalf("Desired().Clear = %v, want %v", got, tc.clear)
			}
		})
	}
}

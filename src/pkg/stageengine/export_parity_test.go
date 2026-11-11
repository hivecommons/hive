package stageengine

import (
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// ParityFacts hands the external parity test (workflow_parity_test.go,
// package stageengine_test — it imports hivedefault, which imports this
// package, so it cannot live here) the Facts corpus the built-in derivers
// are pinned by: every generated PR stage golden plus a sweep of issue
// facts reaching each issue stage. hivecommons/hive#17620.
func ParityFacts() []Facts {
	var out []Facts
	for _, cases := range generateStageGoldens() {
		for _, c := range cases {
			_, f := runStageGoldenWithFacts(c.Input)
			out = append(out, f)
		}
	}
	pr99 := &Ref{Owner: "acme", Repo: "widgets", Number: 99, Kind: stagelabels.PRItem}
	for _, mut := range []func(f *Facts){
		nil,
		func(f *Facts) { f.Issue.Bucket = BucketDependencyDashboard; f.Park.NeedsHuman = true },
		func(f *Facts) { f.Issue.TriageAwaiting = "carries needs-triage" },
		func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Holder: "bob", Kind: "human"}
		},
		func(f *Facts) {
			f.Issue.Bucket = BucketClaimed
			f.Issue.Claim = &ClaimFact{Kind: claims.KindAgent, PRStarted: true}
		},
		func(f *Facts) { f.Issue.Bucket = BucketCoveredByPR; f.Issue.CoveringPR = pr99 },
		func(f *Facts) { f.Park.NeedsHuman = true },
		func(f *Facts) { f.Park.PossiblyExists = true },
		func(f *Facts) { f.Issue.DiscussionLabel = "proposal" },
		func(f *Facts) { f.Open = false },
		func(f *Facts) { f.Issue.Bucket = BucketFiltered },
	} {
		out = append(out, derivedIssueFacts(mut))
	}
	return out
}

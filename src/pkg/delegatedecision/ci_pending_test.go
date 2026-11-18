package delegatedecision

import "testing"

// hivecommons/hive#18889: CIRed() is false while checks are merely pending,
// so a PR whose checks had not finished fell through every rung to the two
// "green head" approvals. Pending CI now answers a refusal that writes
// nothing and keeps the park.
func TestDecidePRPendingCINeverReleases(t *testing.T) {
	cases := []struct {
		name string
		in   Input
	}{
		{"default release", Input{Kind: KindPR, Number: 7, Title: "Add filter", ContributorOnline: true, CIPending: true}},
		{"hive authored", Input{Kind: KindPR, Number: 7, Title: "Add filter", HiveAuthored: true, ContributorOnline: true, CIPending: true}},
		{"small low risk", Input{Kind: KindPR, Number: 7, Title: "Pin the CI image", ContributorOnline: true, CIPending: true, Small: true, LowRiskPaths: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.in)
			if d.Verdict != VerdictRefused || d.Rule != RuleCIPending || !d.KeepsPark || len(d.Actions) != 0 {
				t.Fatalf("pending CI must refuse and write nothing: verdict %s rule %s keeps_park %v actions %+v", d.Verdict, d.Rule, d.KeepsPark, d.Actions)
			}
			if d.Rule == "default-release" || d.Rule == "small-low-risk" {
				t.Fatalf("released a head whose CI is running: %+v", d)
			}
		})
	}
	// The same heads, green, are released as before.
	for _, in := range []Input{
		{Kind: KindPR, Number: 7, Title: "Add filter", ContributorOnline: true, CIGreen: true},
		{Kind: KindPR, Number: 7, Title: "Pin the CI image", ContributorOnline: true, CIGreen: true, Small: true, LowRiskPaths: true},
	} {
		if d := Decide(in); d.Verdict != VerdictApprove {
			t.Fatalf("green head: verdict %s rule %s", d.Verdict, d.Rule)
		}
	}
	// A head with a real problem is still handled by its own rule, pending or not.
	if d := Decide(Input{Kind: KindPR, Number: 7, Title: "Add filter", ContributorOnline: true, CIPending: true, Conflicting: true}); d.Rule == RuleCIPending || d.Verdict == VerdictApprove {
		t.Fatalf("conflicting head must be directed, not waited on or released: rule %s verdict %s", d.Rule, d.Verdict)
	}
}

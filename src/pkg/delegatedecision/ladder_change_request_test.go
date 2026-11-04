package delegatedecision

import (
	"strings"
	"testing"
)

// projectbluefin/testsuite#1057: a fork PR from a clanker, green and
// conflict-free, whose head a maintainer had asked changes on (confirming
// the review swarm's High finding). "Let hive decide" approved it "as it
// is" on the mergeable-as-is rung. A standing change request is a fix the
// decision names, never one it approves over.
func TestLadderStandingChangeRequest(t *testing.T) {
	pr1057 := Input{Repo: "projectbluefin/testsuite", Kind: KindPR, Number: 1057, Author: "mendezr",
		Labels:  []string{"author/clanker", "stage/085-needs-decision"},
		CIGreen: true, ContributorOnline: false, AuthorTooling: AuthorToolingClanker}
	cases := []struct {
		name        string
		in          Input
		verdict     Verdict
		rule        string
		textHas     []string
		ruledOutHas []string
	}{
		{
			name:    "green, clean, nothing requested → still approved as it is",
			in:      pr1057,
			verdict: VerdictApprove, rule: RuleMergeableAsIs,
		},
		{
			name:    "1057: maintainer CHANGES_REQUESTED, author offline → direction naming the request",
			in:      with(pr1057, func(i *Input) { i.ChangesRequestedBy = []string{"Danathar"} }),
			verdict: VerdictDirection, rule: "fix-then-merge",
			textHas: []string{"address the changes @Danathar requested on GitHub"},
			ruledOutHas: []string{
				RuleMergeableAsIs + ": the changes @Danathar requested on GitHub still stand",
				RuleContributorOffline + ": nothing but the changes @Danathar requested on GitHub holds the head back",
			},
		},
		{
			name:    "maintainer CHANGES_REQUESTED, author connected → returned with the request",
			in:      with(pr1057, func(i *Input) { i.ChangesRequestedBy = []string{"Danathar", "castrojo"}; i.ContributorOnline = true }),
			verdict: VerdictDirection, rule: RuleReturnToContributor,
			textHas:     []string{"the author's clanker is connected: address the changes @Danathar, @castrojo requested on GitHub on #1057"},
			ruledOutHas: []string{RuleMergeableAsIs + ": the changes @Danathar, @castrojo requested on GitHub still stand"},
		},
		{
			name:    "only the swarm's changes_requested on the head → not approved",
			in:      with(pr1057, func(i *Input) { i.HeadReviewVerdict = "changes_requested" }),
			verdict: VerdictDirection, rule: "fix-then-merge",
			textHas:     []string{"address the changes the hive's review requested on this head"},
			ruledOutHas: []string{RuleMergeableAsIs + ": the changes the hive's review requested on this head still stand"},
		},
		{
			name:    "small, low-risk head with a change request → the release rules do not approve it either",
			in:      with(pr1057, func(i *Input) { i.ChangesRequestedBy = []string{"Danathar"}; i.Small = true; i.LowRiskPaths = true }),
			verdict: VerdictDirection, rule: "fix-then-merge",
		},
		{
			name: "swarm requires_human on the head → not mergeable-as-is; the reviewer's question is answered by the release rule",
			in:   with(pr1057, func(i *Input) { i.HeadReviewVerdict = "requires_human" }),
			// default-release is the delegate answering the reviewer's
			// question with the reversible default; the rung no longer
			// claims nothing was asked.
			verdict: VerdictApprove, rule: "default-release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if d.Verdict != tc.verdict || d.Rule != tc.rule {
				t.Fatalf("verdict/rule = %s/%s, want %s/%s (%+v)", d.Verdict, d.Rule, tc.verdict, tc.rule, d)
			}
			if tc.in.ChangeRequest() != "" {
				for _, a := range d.Actions {
					if a.Kind == ActionApproveReview {
						t.Fatalf("approved over a standing change request: %+v", d)
					}
				}
			}
			for _, w := range tc.textHas {
				if !strings.Contains(d.Text, w) {
					t.Errorf("text %q lacks %q", d.Text, w)
				}
			}
			if len(tc.ruledOutHas) == 0 {
				return
			}
			if d.Ladder == nil {
				t.Fatalf("no ladder record on %+v", d)
			}
			joined := strings.Join(d.Ladder.RuledOut, "\n")
			for _, w := range tc.ruledOutHas {
				if !strings.Contains(joined, w) {
					t.Errorf("ruled_out lacks %q:\n%s", w, joined)
				}
			}
		})
	}
}

// The requires_human rule-out is recorded too, so the operator sees why
// the rung did not fire.
func TestLadderHeadNeedsPersonRuledOut(t *testing.T) {
	in := Input{Kind: KindPR, Number: 1, CIGreen: true, ContributorOnline: true, HeadReviewVerdict: "requires_human"}
	if got := mergeBlockers(in); !strings.Contains(got, "the hive's review on this head asks a person") {
		t.Fatalf("mergeBlockers = %q", got)
	}
}

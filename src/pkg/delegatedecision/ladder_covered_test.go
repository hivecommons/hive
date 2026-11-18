package delegatedecision

import (
	"strings"
	"testing"
)

// hivecommons/hive#18900: on #18866 the hive adopted contributor PR #18870 as
// #18879 while the owner's #18873 for the same issue was open. A person's or
// the hive's open PR (or claim) on the issue closes the contributor PR as a
// duplicate with credit; "green first, then earlier-opened" never lets a
// contributor copy win over it.
func TestLadderCoveredByOtherPR(t *testing.T) {
	// The #18870 scenario: an offline contributor clanker, CI red, which rung
	// 5 would adopt.
	adoptable := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 18870, Title: "Fix the hold", Author: "Karibhou",
		Labels: []string{"author/clanker"}, CIGreen: false, ContributorOnline: false, AuthorTooling: AuthorToolingClanker}
	if d, ok := decideLadder(adoptable, "#18870"); !ok || d.Verdict != VerdictAdoptSupersede {
		t.Fatalf("control: the uncovered PR must be adopted, got %+v (ok %v)", d, ok)
	}

	covered := func(mut func(*Input)) Input { return with(adoptable, mut) }
	cases := []struct {
		name    string
		in      Input
		verdict Verdict
		target  int
		textHas string
	}{
		{
			name:    "owner PR open for the issue: closed as a duplicate of it with credit",
			in:      covered(func(i *Input) { i.Competing = []CompetingPR{{Number: 18873, PersonAuthored: true}} }),
			verdict: VerdictCloseSuperseded, target: 18873, textHas: "superseded by #18873 with credit: a person's PR",
		},
		{
			name:    "hive PR open for the issue: closed as a duplicate of it",
			in:      covered(func(i *Input) { i.Competing = []CompetingPR{{Number: 18873, HiveAuthored: true}} }),
			verdict: VerdictCloseSuperseded, target: 18873, textHas: "the hive's PR already covers",
		},
		{
			name: "a conflicting contributor head still loses to the person's PR it competes with",
			in: covered(func(i *Input) {
				i.Competing = []CompetingPR{{Number: 18873, PersonAuthored: true}}
				i.CIGreen, i.Conflicting = false, true
			}),
			verdict: VerdictCloseSuperseded, target: 18873,
		},
		{
			name: "paused repo: recommend only, no write",
			in: covered(func(i *Input) {
				i.Competing = []CompetingPR{{Number: 18873, PersonAuthored: true}}
				i.RepoPaused = true
			}),
			verdict: VerdictRecommendOnly, textHas: "close #18870 as a duplicate of #18873",
		},
		{
			name:    "a person's claim on the issue and no PR yet: not adopted, the holder is named",
			in:      covered(func(i *Input) { i.IssueClaimHolder, i.IssueClaimIssue = "andy", 18866 }),
			verdict: VerdictRecommendOnly, textHas: "andy has claimed issue #18866",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := decideLadder(tc.in, "#18870")
			if !ok || d.Rule != RuleCoveredByOtherPR || d.Verdict != tc.verdict {
				t.Fatalf("decision = %+v (ok %v), want %s / %s", d, ok, RuleCoveredByOtherPR, tc.verdict)
			}
			for _, a := range d.Actions {
				if a.Kind == ActionAdoptSupersede {
					t.Fatalf("a covered PR must never be adopted: %+v", d.Actions)
				}
			}
			if tc.target > 0 {
				if len(d.Actions) != 1 || d.Actions[0].Kind != ActionCloseDuplicate || d.Actions[0].Target != tc.target {
					t.Fatalf("actions = %+v, want one close-duplicate of #%d", d.Actions, tc.target)
				}
				if !strings.Contains(d.Actions[0].Text, "Thank you for this PR") || !strings.Contains(d.Actions[0].Text, "credited") {
					t.Fatalf("the close note must thank and credit the contributor: %q", d.Actions[0].Text)
				}
			}
			if tc.textHas != "" && !strings.Contains(d.Text, tc.textHas) {
				t.Fatalf("text %q missing %q", d.Text, tc.textHas)
			}
			if d.Ladder == nil || d.Ladder.Decision != RuleCoveredByOtherPR {
				t.Fatalf("ladder note missing: %+v", d.Ladder)
			}
			if AdoptsImmediately(tc.in) {
				t.Fatal("AdoptsImmediately must be false for a covered PR: the nudge side must not promise an adoption")
			}
		})
	}

	// Equal trust is a tie, not cover: another contributor's PR, or the
	// competitor the merge queue holds as this PR's own duplicate, do not stop
	// the adoption.
	for name, comp := range map[string][]CompetingPR{
		"another contributor's PR":         {{Number: 18890}},
		"a duplicate held behind this one": {{Number: 18891, PersonAuthored: true, HeldDuplicate: true}},
	} {
		t.Run("not covered: "+name, func(t *testing.T) {
			d, ok := decideLadder(covered(func(i *Input) { i.Competing = comp }), "#18870")
			if !ok || d.Verdict != VerdictAdoptSupersede {
				t.Fatalf("decision = %+v (ok %v), want adoption to proceed", d, ok)
			}
		})
	}
}

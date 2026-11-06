package delegatedecision

import (
	"strings"
	"testing"
)

// projectbluefin/utah#919 (fork-rehome-refused on its head, author relay
// offline since 10-26) and projectbluefin/testsuite#1037 (a fork changing
// .github/workflows/e2e.yml; the fixer's pushes were refused) were decided
// "hive's fixer: … then it merges" on branches the hive cannot push. The
// fixer gets the direction only when it can push; otherwise a connected
// author does, else the person who asked.
func TestDecideAdoptedPushability(t *testing.T) {
	base := Input{Kind: KindPR, Number: 919, Author: "arteeh", AdoptedByHive: true,
		Reasons: []string{"reviewer-verdict:requires_human"}, ReviewerRequiresHuman: true,
		ReviewerFinding: "address the changes @hanthor requested", CIGreen: true}
	const refused = "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	for _, tc := range []struct {
		name      string
		refused   string
		online    bool
		rule      string
		keepsPark bool
		comment   bool
	}{
		{name: "pushable, author offline: the hive's fixer", rule: RuleAdoptedFixer, comment: true},
		{name: "pushable, author online: still the hive's fixer", online: true, rule: RuleAdoptedFixer, comment: true},
		{name: "not pushable, author online: back to the author", refused: refused, online: true, rule: RuleReturnToContributor, comment: true},
		{name: "not pushable, author offline: the person who asked", refused: refused, rule: RuleAdoptedPushRefused, keepsPark: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := with(base, func(i *Input) { i.AdoptedPushRefused = tc.refused; i.ContributorOnline = tc.online })
			d := Run(in)
			if d.Rule != tc.rule || d.KeepsPark != tc.keepsPark {
				t.Fatalf("rule %s keepsPark %v, want %s %v: %q", d.Rule, d.KeepsPark, tc.rule, tc.keepsPark, d.Text)
			}
			hasComment := false
			for _, a := range d.Actions {
				if a.Kind == ActionCommentDecision {
					hasComment = true
				}
			}
			if hasComment != tc.comment {
				t.Fatalf("decision comment = %v, want %v (actions %+v)", hasComment, tc.comment, d.Actions)
			}
			if tc.refused != "" && (!strings.Contains(d.Text+d.Rationale, "fork-rehome-refused") || strings.Contains(d.Text, "hive's fixer")) {
				t.Fatalf("a refused push must name why and never promise the fixer: text %q rationale %q", d.Text, d.Rationale)
			}
		})
	}
}

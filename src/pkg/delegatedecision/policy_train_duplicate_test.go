package delegatedecision

import (
	"strings"
	"testing"
)

// Two open PRs for one issue (hivecommons/hive#15395): the merge queue's
// pick stands. Deciding on the held PR closes it as superseded by the
// kept one; deciding on the kept PR releases it (or, if it has gone red,
// only recommends and names the fix).
func TestDecideTrainDuplicate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      Input
		verdict Verdict
		target  int
		textHas string
	}{
		{
			name:    "held later PR closes as superseded by the earlier green one",
			in:      Input{Kind: KindPR, Number: 15407, Title: "fix: x", CIGreen: true, TrainDuplicateOf: 15402, TrainDuplicateIssue: 15396},
			verdict: VerdictCloseSuperseded, target: 15402, textHas: "superseded by #15402: both PRs fix the same issue for issue #15396",
		},
		{
			name:    "held earlier (red) PR closes as superseded by the later green one",
			in:      Input{Kind: KindPR, Number: 15402, Title: "fix: x", CIGreen: false, TrainDuplicateOf: 15407, TrainDuplicateIssue: 15396},
			verdict: VerdictCloseSuperseded, target: 15407, textHas: "superseded by #15407",
		},
		{
			name:    "held PR on a paused repo: recommend only",
			in:      Input{Kind: KindPR, Number: 15407, Title: "fix: x", CIGreen: true, RepoPaused: true, TrainDuplicateOf: 15402},
			verdict: VerdictRecommendOnly, textHas: "close #15407 as superseded by #15402",
		},
		{
			name:    "kept green PR is released and names the held duplicate",
			in:      Input{Kind: KindPR, Number: 15402, Title: "fix: x", CIGreen: true, HiveAuthored: true, TrustedAuthor: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 15407, HeldDuplicate: true, Mergeable: true, Current: true}}},
			verdict: VerdictApprove, textHas: "#15407 is held as its duplicate",
		},
		{
			name:    "kept PR gone red: recommend the fix, or deciding on the held one",
			in:      Input{Kind: KindPR, Number: 15402, Title: "fix: x", CIGreen: false, HiveAuthored: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 15407, HeldDuplicate: true}}},
			verdict: VerdictRecommendOnly, textHas: "decide on #15407 to let it supersede this one",
		},
		{
			name:    "a plain competitor is not a queue duplicate",
			in:      Input{Kind: KindPR, Number: 15402, Title: "fix: x", CIGreen: true, HiveAuthored: true, TrustedAuthor: true, ContributorOnline: true, Competing: []CompetingPR{{Number: 15407, HiveAuthored: true, Mergeable: true}}},
			verdict: VerdictApprove,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.in)
			if d.Verdict != tc.verdict {
				t.Fatalf("verdict = %s (%s: %s), want %s", d.Verdict, d.Rule, d.Rationale, tc.verdict)
			}
			if tc.name != "a plain competitor is not a queue duplicate" && d.Rule != RuleOnePRPerIssue {
				t.Fatalf("rule = %s, want %s", d.Rule, RuleOnePRPerIssue)
			}
			if tc.target > 0 {
				var closed []Action
				for _, a := range d.Actions {
					if a.Kind == ActionCloseDuplicate {
						closed = append(closed, a)
					}
				}
				if len(closed) != 1 || closed[0].Target != tc.target || !strings.Contains(closed[0].Text, "credited on the issue") {
					t.Fatalf("close action = %+v, want target #%d with credit", closed, tc.target)
				}
			}
			words := d.Text + "\n" + d.Rationale
			if tc.textHas != "" && !strings.Contains(words, tc.textHas) {
				t.Fatalf("words %q lack %q", words, tc.textHas)
			}
		})
	}
}

package delegatedecision

import (
	"strings"
	"testing"
)

// TestDecideHiveSuperseder: the owner's rule in the delegate — the hive's
// superseding PR is released with no decision; a hard stop holds it,
// and the recommendation names it.
func TestDecideHiveSuperseder(t *testing.T) {
	base := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 15649, Author: "hivecommons-hive[bot]",
		Body: "Supersedes #15646", HiveAuthored: true, TrustedAuthor: true, HiveSuperseding: true, CIGreen: true,
		Reasons: []string{"verdict:requires_human"}}

	d := Decide(base)
	if d.Verdict != VerdictApprove || d.Rule != RuleHiveSuperseder || !strings.Contains(d.Text, "auto-merging once CI/review are clean") {
		t.Fatalf("decision = %+v, want release under the superseder rule", d)
	}

	stopped := base
	stopped.HiveSupersederHardStop = "sentinel-unacknowledged"
	stopped.SentinelAlert = true
	d = Decide(stopped)
	if d.Verdict != VerdictRecommendOnly || d.Rule != RuleHiveSuperseder || !strings.Contains(d.Text, "sentinel-unacknowledged") {
		t.Fatalf("decision = %+v, want recommend-only naming the stop", d)
	}

	// An unacknowledged sentinel with no stop token recorded still defers
	// to the sentinel rules rather than releasing.
	sentinel := base
	sentinel.SentinelAlert = true
	if d = Decide(sentinel); d.Rule == RuleHiveSuperseder {
		t.Fatalf("decision = %+v, sentinel must not be released by the superseder rule", d)
	}

	plain := base
	plain.HiveSuperseding = false
	if d = Decide(plain); d.Rule == RuleHiveSuperseder {
		t.Fatalf("decision = %+v, a non-superseding PR takes the ordinary rules", d)
	}
}

func TestDecideHiveSupersederDecisionQuestion(t *testing.T) {
	const question = "Should this verified change receive sign-off?\nPlease approve or request changes."
	for _, tc := range []struct {
		name     string
		question string
		signOff  bool
		stop     string
		sentinel bool
		want     Verdict
		contains string
	}{
		{name: "sign-off only", question: " \t" + question + "\n", signOff: true, want: VerdictRecommendOnly, contains: question},
		{name: "scope question", question: "@clubanderson should confirm the merge-request relay may lift the hold…", want: VerdictApprove, contains: "auto-merging once CI/review are clean"},
		{name: "scope question auto-decides", question: "Should this merge ahead of the docs follow-up?", want: VerdictApprove, contains: "auto-merging once CI/review are clean"},
		{name: "unclassified wording does not imply sign-off", question: question, want: VerdictApprove, contains: "auto-merging once CI/review are clean"},
		{name: "whitespace only", question: " \t\n", signOff: true, want: VerdictApprove, contains: "auto-merging once CI/review are clean"},
		{name: "hard stop takes precedence", question: question, signOff: true, stop: "sentinel-unacknowledged", sentinel: true, want: VerdictRecommendOnly, contains: "clear the stop"},
		{name: "scope question with hard stop", question: "Should the docs update be a follow-up?", stop: "ci-red", want: VerdictRecommendOnly, contains: "clear the stop"},
		{name: "question with sentinel", question: question, signOff: true, sentinel: true, want: VerdictRecommendOnly, contains: question},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 16607, Author: "hivecommons-hive[bot]",
				Body: "Supersedes #16456", HiveAuthored: true, TrustedAuthor: true, HiveSuperseding: true, CIGreen: true,
				Question: tc.question, SignOffQuestion: tc.signOff, HiveSupersederHardStop: tc.stop, SentinelAlert: tc.sentinel,
				Reasons: []string{"decision-requested"}}
			d := Run(in)
			if d.Verdict != tc.want || d.Rule != RuleHiveSuperseder || !strings.Contains(d.Text, tc.contains) {
				t.Fatalf("decision = %+v, want %s under the superseder rule naming %q", d, tc.want, tc.contains)
			}
			if tc.want == VerdictRecommendOnly {
				if !d.KeepsPark || len(d.Actions) != 1 || d.Actions[0].Kind != ActionRecommendOnly || d.Actions[0].Text != d.Text {
					t.Fatalf("decision = %+v, want only a recommendation keeping the park", d)
				}
				if tc.stop != "" && (!strings.Contains(d.Text, tc.stop) || strings.Contains(d.Text, question)) {
					t.Fatalf("decision = %+v, want the hard stop to take precedence over the question", d)
				}
			} else if d.KeepsPark || len(d.Actions) != 2 || d.Actions[1].Kind != ActionApproveReview {
				t.Fatalf("decision = %+v, want approval with no park", d)
			}
		})
	}
}

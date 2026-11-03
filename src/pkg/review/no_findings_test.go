package review

import (
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

func TestNoFindingsAcknowledgement(t *testing.T) {
	yes := []string{
		"**Reviewed** — no findings from this perspective.",
		"**Reviewed** — no findings from security, correctness.",
		"**Reviewed** — no findings from adjudicator (all 2 review heads agreed: abc1234, def5678).",
		"  Reviewed - no findings from this perspective  \n",
		"_Reviewed_ – no findings from maintainability",
	}
	for _, b := range yes {
		if !NoFindingsAcknowledgement(b) {
			t.Errorf("should match: %q", b)
		}
	}
	no := []string{
		"",
		"**Reviewed** — no findings from security.\n\n**correctness**\n- src/x.go:12 nil deref",
		"**security**\n- src/x.go:12 nil deref\n\n_No findings from: style._",
		"No findings from this perspective, but one nit: …",
	}
	for _, b := range no {
		if NoFindingsAcknowledgement(b) {
			t.Errorf("should not match: %q", b)
		}
	}
}

func TestCleanReview(t *testing.T) {
	approve := []PerspectiveReport{{Perspective: PerspectiveCorrectness, Verdict: VerdictApprove}}
	narrated := "Read the tree at `c7c8dfb`: the retry loop bounds its attempts and the error is wrapped. Looks correct to me.\n\nNo findings."
	cases := []struct {
		name    string
		body    string
		reports []PerspectiveReport
		want    bool
	}{
		{"one-line ack without reports", "**Reviewed** — no findings from this perspective.", nil, true},
		{"narrated clean reviewer-lane body", narrated, approve, true},
		{"no further findings", "Re-read after the fixup; no further findings from me.", approve, true},
		{"not-applicable perspective", "Nothing to review for style here — no findings.", []PerspectiveReport{{Perspective: PerspectiveStyle, Verdict: VerdictApprove, NotApplicable: true}}, true},
		{"narration without the phrase", "Read the tree at `c7c8dfb`. Looks correct to me.", approve, false},
		{"says no findings but report has one", narrated, []PerspectiveReport{{AgentReport: outputschema.AgentReport{Findings: []outputschema.Finding{{Title: "nil deref"}}}, Perspective: PerspectiveCorrectness, Verdict: VerdictApprove}}, false},
		{"says no findings but requests changes", narrated, []PerspectiveReport{{Perspective: PerspectiveCorrectness, Verdict: VerdictChangesRequested}}, false},
		{"decision requested", "No findings, but a maintainer should decide the API shape.", []PerspectiveReport{{Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, DecisionRequested: true}}, false},
		{"hidden marker", "<!-- hive-decision-question: keep the flag? -->\nNo findings otherwise.", approve, false},
		{"narrated body without reports", narrated, nil, false},
		{"empty", "", approve, false},
	}
	for _, tc := range cases {
		if got := CleanReview(tc.body, tc.reports); got != tc.want {
			t.Errorf("%s: CleanReview = %v, want %v", tc.name, got, tc.want)
		}
	}
}

package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

func finding(sev outputschema.Severity) outputschema.Finding {
	return outputschema.Finding{Title: string(sev), Severity: sev, Summary: "s"}
}

// The score is a pure function of verdicts, severities and coverage. Each row
// pins one rule so a change to the weights is a deliberate, visible edit.
func TestScoreConfidenceMatrix(t *testing.T) {
	n := len(DefaultPerspectives)
	withFirst := func(v Verdict, fs ...outputschema.Finding) []PerspectiveReport {
		reps := allApprove()
		reps[0] = baseReport(DefaultPerspectives[0], v, fs...)
		return reps
	}
	tests := []struct {
		name   string
		reps   []PerspectiveReport
		want   int
		reason string
	}{
		{"clean unanimous approve is max", allApprove(), ConfidenceMax, ""},
		{"info and low findings cost nothing", withFirst(VerdictApprove, finding(outputschema.SeverityInfo), finding(outputschema.SeverityLow)), ConfidenceMax, ""},
		{"medium deducts one", withFirst(VerdictApprove, finding(outputschema.SeverityMedium)), 4, "1 medium finding"},
		{"two mediums deduct two", withFirst(VerdictApprove, finding(outputschema.SeverityMedium), finding(outputschema.SeverityMedium)), 3, "2 medium findings"},
		{"high deducts two", withFirst(VerdictApprove, finding(outputschema.SeverityHigh)), 3, "1 high finding"},
		{"critical floors at zero", withFirst(VerdictApprove, finding(outputschema.SeverityCritical)), 0, "1 critical finding"},
		{"changes requested caps at needs-attention", withFirst(VerdictChangesRequested), ConfidenceNeedsAttentionCap, "1 perspective requested changes"},
		{"requires human caps at needs-attention", withFirst(VerdictRequiresHuman), ConfidenceNeedsAttentionCap, "1 perspective requires a human decision"},
		{"reject is zero", withFirst(VerdictReject), ConfidenceRejectScore, "reject verdict"},
		{"cap does not lift a lower deduction", withFirst(VerdictChangesRequested, finding(outputschema.SeverityHigh), finding(outputschema.SeverityHigh)), 1, "2 high findings"},
		{"missing perspective caps coverage", allApprove()[:n-1], ConfidenceCoverageCap, "4 of 5 perspectives reported"},
		{"no reports is zero", nil, ConfidenceRejectScore, "no review reports"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScoreConfidence(tt.reps, n)
			if got.Score != tt.want {
				t.Fatalf("score = %d, want %d (reasons %v)", got.Score, tt.want, got.Reasons)
			}
			if tt.reason == "" && len(got.Reasons) != 0 {
				t.Fatalf("expected no reasons, got %v", got.Reasons)
			}
			if tt.reason != "" && !strings.Contains(strings.Join(got.Reasons, ";"), tt.reason) {
				t.Fatalf("reasons %v lack %q", got.Reasons, tt.reason)
			}
		})
	}
}

// expected == 0 means the caller has no configured set to judge coverage by;
// a single-perspective hive must not be capped for being single.
func TestScoreConfidenceNoCoverageJudgementWhenExpectedUnknown(t *testing.T) {
	got := ScoreConfidence([]PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictApprove)}, 0)
	if got.Score != ConfidenceMax || len(got.Reasons) != 0 {
		t.Fatalf("got %+v, want max with no reasons", got)
	}
}

func TestConfidenceBandAndRender(t *testing.T) {
	for score, band := range map[int]string{5: "safe", 4: "safe", 3: "needs attention", 1: "needs attention", 0: "do not merge"} {
		if got := ConfidenceBand(score); got != band {
			t.Errorf("band(%d) = %q, want %q", score, got, band)
		}
	}
	if got := (Confidence{Score: 5}).Render(); got != "**Confidence: 5/5** (safe)" {
		t.Errorf("clean render = %q", got)
	}
	got := (Confidence{Score: 3, Reasons: []string{"1 high finding", "1 perspective requested changes"}}).Render()
	if got != "**Confidence: 3/5** (needs attention) — 1 high finding; 1 perspective requested changes" {
		t.Errorf("render = %q", got)
	}
}

// The aggregate carries the score so review-verdicts.json and the comment
// line are computed from the same reports and can never disagree.
func TestAggregateReportsCarriesConfidence(t *testing.T) {
	agg := AggregateReports(allApprove(), AggregateOptions{})
	if agg.Confidence.Score != ConfidenceMax {
		t.Fatalf("clean aggregate confidence = %+v", agg.Confidence)
	}
	reps := allApprove()
	reps[1] = baseReport(DefaultPerspectives[1], VerdictApprove, finding(outputschema.SeverityHigh))
	agg = AggregateReports(reps, AggregateOptions{})
	if agg.Verdict != VerdictRequiresHuman || agg.Confidence.Score != 3 {
		t.Fatalf("verdict %s confidence %+v, want requires_human / 3", agg.Verdict, agg.Confidence)
	}
}

package review

import (
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// A reviewer can ask for changes with no structured finding and explain
// itself only in its summary ("still present at this head"). The aggregate
// keeps that summary, so a PR returned to its author can say what the
// reviewer said. An approving perspective's summary is not kept, and the
// summaries never become Reasons: nothing routes or re-records on them.
func TestAggregateKeepsBlockingPerspectiveSummaries(t *testing.T) {
	blocking := baseReport(PerspectiveCorrectness, VerdictChangesRequested)
	blocking.Summary = "the early return in the handler is\nstill present at this head"
	approving := baseReport(PerspectiveSecurity, VerdictApprove)
	approving.Summary = "nothing to add"

	agg := AggregateReports([]PerspectiveReport{blocking, approving}, AggregateOptions{})
	if agg.Verdict != VerdictChangesRequested {
		t.Fatalf("verdict = %s, want changes_requested", agg.Verdict)
	}
	want := []string{"correctness: the early return in the handler is\nstill present at this head"}
	if !slices.Equal(agg.BlockSummaries, want) {
		t.Fatalf("BlockSummaries = %q, want %q", agg.BlockSummaries, want)
	}
	for _, r := range agg.Reasons {
		if strings.Contains(r, "still present") {
			t.Fatalf("a blocking summary became a reason: %q", agg.Reasons)
		}
	}
	if got := agg.BlockSummaryAsks(0); len(got) != 1 || got[0] != "correctness: the early return in the handler is still present at this head" {
		t.Fatalf("BlockSummaryAsks = %q, want the summary on one line", got)
	}

	// The same report recorded twice (a re-collect) is listed once, and a
	// blocking report with no summary adds nothing.
	silent := baseReport(PerspectiveSecurity, VerdictChangesRequested)
	silent.Summary = "  "
	agg = AggregateReports([]PerspectiveReport{blocking, blocking, silent}, AggregateOptions{})
	if !slices.Equal(agg.BlockSummaries, want) {
		t.Fatalf("BlockSummaries = %q, want %q once", agg.BlockSummaries, want)
	}

	// All approve: nothing blocks, nothing is kept.
	if agg := AggregateReports(allApprove(), AggregateOptions{}); len(agg.BlockSummaries) != 0 || len(agg.BlockSummaryAsks(0)) != 0 {
		t.Fatalf("an approving aggregate kept blocking summaries: %q", agg.BlockSummaries)
	}

	// A report whose only findings are out of scope does not block
	// (ScopeEffectiveReports), so its summary is not a blocking one.
	outOfScope := baseReport(PerspectiveCorrectness, VerdictChangesRequested, outputschema.Finding{Title: "unrelated cleanup", Severity: outputschema.SeverityLow, ReviewScope: "out-of-scope"})
	outOfScope.Summary = "an unrelated file could be tidied"
	if agg := AggregateReports([]PerspectiveReport{outOfScope}, AggregateOptions{}); len(agg.BlockSummaries) != 0 {
		t.Fatalf("an out-of-scope-only report's summary was kept as blocking: %q", agg.BlockSummaries)
	}
}

// BlockSummaryAsks renders one trimmed line per summary: the
// changes_requested / reject perspectives first, then the perspectives that
// asked for a person, with the same limit and "and N more" line as the
// finding asks.
func TestBlockSummaryAsksTrimsOrdersAndLimits(t *testing.T) {
	long := "correctness: " + strings.Repeat("x", 400)
	agg := Aggregate{
		BlockSummaries: []string{long, "quality:  the  test\tdoes not cover the new branch ", " "},
		HumanSummaries: []string{"security requires human: the token scope is wider than the change needs", "security requires human: the token scope is wider than the change needs"},
	}
	got := agg.BlockSummaryAsks(0)
	if len(got) != 3 {
		t.Fatalf("BlockSummaryAsks = %q, want three lines", got)
	}
	if n := len([]rune(got[0])); n != findingAskMaxRunes || !strings.HasSuffix(got[0], "…") {
		t.Fatalf("long summary is %d runes (%q), want it cut to %d", n, got[0], findingAskMaxRunes)
	}
	if got[1] != "quality: the test does not cover the new branch" {
		t.Fatalf("second line = %q, want whitespace collapsed", got[1])
	}
	if got[2] != "security requires human: the token scope is wider than the change needs" {
		t.Fatalf("third line = %q, want the requires_human summary once, after the blocking ones", got[2])
	}
	limited := agg.BlockSummaryAsks(2)
	if len(limited) != 3 || limited[1] != got[1] || limited[2] != "and 1 more reviewer summary line(s) in the review" {
		t.Fatalf("limited BlockSummaryAsks = %q", limited)
	}
	if got := (Aggregate{}).BlockSummaryAsks(5); len(got) != 0 {
		t.Fatalf("an aggregate with no summaries rendered %q", got)
	}
}

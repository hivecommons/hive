package evidence

import (
	"strings"
	"testing"
)

func fptr(f float64) *float64 { return &f }

func words(s string) int { return len(strings.Fields(s)) }

func TestLetterFromScore(t *testing.T) {
	for _, tc := range []struct {
		score float64
		want  string
	}{
		{1.0, "A+"}, {0.97, "A+"}, {0.969, "A"}, {0.93, "A"}, {0.9, "A-"},
		{0.87, "B+"}, {0.83, "B"}, {0.8, "B-"}, {0.77, "C+"}, {0.73, "C"}, {0.72, "C-"}, {0, "C-"}, {-1, "C-"},
	} {
		if got := letterFromScore(tc.score); got != tc.want {
			t.Errorf("letterFromScore(%v) = %q, want %q", tc.score, got, tc.want)
		}
	}
}

func TestBandOf(t *testing.T) {
	for letter, want := range map[string]string{"A+": BandA, "A-": BandA, "B": BandB, "C-": BandC, NoGradeLetter: BandNone, "": BandNone} {
		if got := BandOf(letter); got != want {
			t.Errorf("BandOf(%q) = %q, want %q", letter, got, want)
		}
	}
}

func TestGradeOutcome(t *testing.T) {
	cases := []struct {
		name  string
		stats []OutcomeStats
		want  string
	}{
		{"empty", nil, NoGradeLetter},
		{"too few known", []OutcomeStats{{Kind: AuthorAgent, N: 19, Merged: 19}}, NoGradeLetter},
		{"unknown does not count", []OutcomeStats{{Kind: AuthorAgent, N: 40, Merged: 10, Unknown: 30}}, NoGradeLetter},
		{"all merged no reverts", []OutcomeStats{{Kind: AuthorAgent, N: 20, Merged: 20}}, "A+"},
		{"across kinds", []OutcomeStats{{Kind: AuthorAgent, N: 15, Merged: 15}, {Kind: AuthorHuman, N: 5, Merged: 4, Closed: 1}}, "A"},
		{"reverts discount", []OutcomeStats{{Kind: AuthorAgent, N: 20, Merged: 20, Reverted: 2}}, "A-"},
		{"low merge share", []OutcomeStats{{Kind: AuthorAgent, N: 40, Merged: 24, Closed: 16}}, "C-"},
	}
	for _, tc := range cases {
		g := GradeOutcome(tc.stats)
		if g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
		if g.EnoughData != (tc.want != NoGradeLetter) || g.Band != BandOf(tc.want) || g.Reason == "" || g.Rubric == "" {
			t.Errorf("%s: inconsistent grade %+v", tc.name, g)
		}
	}
}

func TestSummarizeOutcome(t *testing.T) {
	if got := SummarizeOutcome(nil); got != "No PRs with an evidence record in this window." {
		t.Errorf("empty: %q", got)
	}
	got := SummarizeOutcome([]OutcomeStats{
		{Kind: AuthorHuman, N: 2, Merged: 2},
		{Kind: AuthorAgent, N: 20, Merged: 17, Unknown: 3},
		{Kind: AuthorBot, N: 1, Merged: 1},
	})
	want := "Agents: 17 merged, 0 reverted. Humans: 2 merged, 0 reverted. Bots: 1 merged, 0 reverted. 3 outcomes not recorded yet."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if words(got) > PlainSummaryMaxWords {
		t.Errorf("summary has %d words: %q", words(got), got)
	}
}

func TestGradeCalibration(t *testing.T) {
	cases := []struct {
		name    string
		buckets []CalibrationBucket
		want    string
	}{
		{"empty", nil, NoGradeLetter},
		{"top bucket thin", []CalibrationBucket{{"0.0–0.5", 50, 10}, {"0.9–1.0", 19, 19}}, NoGradeLetter},
		{"rising", []CalibrationBucket{{"0.0–0.5", 10, 3}, {"0.5–0.7", 10, 6}, {"0.9–1.0", 30, 28}}, "A"},
		{"not monotonic loses ten points", []CalibrationBucket{{"0.0–0.5", 10, 10}, {"0.5–0.7", 10, 6}, {"0.9–1.0", 30, 28}}, "B"},
		{"thin low bucket ignored", []CalibrationBucket{{"0.0–0.5", 2, 2}, {"0.5–0.7", 10, 6}, {"0.9–1.0", 30, 28}}, "A"},
		{"weak top", []CalibrationBucket{{"0.9–1.0", 40, 20}}, "C-"},
	}
	for _, tc := range cases {
		if g := GradeCalibration(tc.buckets); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
	if !strings.Contains(GradeCalibration([]CalibrationBucket{{"0.0–0.5", 10, 10}, {"0.9–1.0", 30, 28}}).Reason, "does not rise") {
		t.Error("non-monotonic reason should say so")
	}
}

func TestSummarizeCalibration(t *testing.T) {
	for _, tc := range []struct {
		buckets []CalibrationBucket
		want    string
	}{
		{nil, "No reviewer verdicts with a known PR outcome in this window yet."},
		{[]CalibrationBucket{{"0.0–0.5", 4, 1}, {"0.9–1.0", 0, 0}}, "No high-confidence verdicts yet; 4 lower-confidence verdicts have a known outcome."},
		{[]CalibrationBucket{{"0.0–0.5", 4, 1}, {"0.9–1.0", 40, 34}}, "When reviewers were most sure (0.9–1.0), 85% of 40 PRs merged."},
	} {
		if got := SummarizeCalibration(tc.buckets); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeAccuracy(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   AccuracyStats
		want string
	}{
		{"ledger missing", AccuracyStats{Available: false, BundleVerdicts: 500}, NoGradeLetter},
		{"few samples", AccuracyStats{Available: true, Samples: 19}, NoGradeLetter},
		{"clean", AccuracyStats{Available: true, Samples: 120, FalseApproveRate: 0.02, FalseBlockRate: 0.01}, "A+"},
		{"worse rate wins", AccuracyStats{Available: true, Samples: 120, FalseApproveRate: 0.02, FalseBlockRate: 0.15}, "B"},
		{"bad", AccuracyStats{Available: true, Samples: 120, FalseApproveRate: 0.4}, "C-"},
	} {
		if g := GradeAccuracy(tc.in); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
	if r := GradeAccuracy(AccuracyStats{}).Reason; !strings.Contains(r, "ledger") {
		t.Errorf("missing-ledger reason should name the ledger: %q", r)
	}
}

func TestSummarizeAccuracy(t *testing.T) {
	for _, tc := range []struct {
		in   AccuracyStats
		want string
	}{
		{AccuracyStats{}, "No reviewer verdicts in this window yet."},
		{AccuracyStats{BundleVerdicts: 1}, "1 reviewer verdict recorded; accuracy is graded once the outcome ledger covers them."},
		{AccuracyStats{Available: true, Samples: 120, FalseApproveRate: 0.03, FalseBlockRate: 0.011}, "Reviewers wrongly approved 3% and wrongly blocked 1% of 120 PRs with a known result."},
	} {
		if got := SummarizeAccuracy(tc.in); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeMaintainability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		trends []ComplexityTrend
		want   string
	}{
		{"none", nil, NoGradeLetter},
		{"both down", []ComplexityTrend{{"o/r", 4, 3, 10, 8}}, "A+"},
		{"flat", []ComplexityTrend{{"o/r", 3, 3, 10, 10}}, "A-"},
		{"one down one flat", []ComplexityTrend{{"o/r", 3, 3, 10, 9}}, "A"},
		{"one up one down", []ComplexityTrend{{"o/r", 3, 4, 10, 9}}, "B+"},
		{"one up one flat", []ComplexityTrend{{"o/r", 3, 3, 10, 12}}, "B"},
		{"both up", []ComplexityTrend{{"o/r", 3, 4, 10, 12}}, "C"},
		{"summed across repos", []ComplexityTrend{{"o/a", 3, 4, 10, 12}, {"o/b", 5, 3, 20, 10}}, "A+"},
	} {
		if g := GradeMaintainability(tc.trends); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
}

func TestSummarizeMaintainability(t *testing.T) {
	for _, tc := range []struct {
		trends []ComplexityTrend
		repos  int
		want   string
	}{
		{nil, 0, "No monthly code-complexity snapshots in this window."},
		{nil, 1, "1 repo has one complexity snapshot; a trend needs two months."},
		{nil, 2, "2 repos have one complexity snapshot; a trend needs two months."},
		{[]ComplexityTrend{{"o/a", 3, 3, 8, 7}, {"o/b", 5, 5, 6, 5}, {"o/c", 2, 2, 0, 0}}, 3, "Code complexity across 3 repos: median flat, 12 complex functions (was 14)."},
		{[]ComplexityTrend{{"o/a", 3, 5, 0, 1}}, 1, "Code complexity across 1 repo: median rising, 1 complex function (was 0)."},
	} {
		if got := SummarizeMaintainability(tc.trends, tc.repos); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   BoundaryStats
		want string
	}{
		{"too few", BoundaryStats{TotalWrites: 19, Blocked: 1}, NoGradeLetter},
		{"never exercised", BoundaryStats{TotalWrites: 1000}, "A-"},
		{"quiet boundary", BoundaryStats{TotalWrites: 1000, Blocked: 3}, "A+"},
		{"two percent", BoundaryStats{TotalWrites: 1000, Blocked: 20}, "A"},
		{"five percent", BoundaryStats{TotalWrites: 1000, Blocked: 50}, "B"},
		{"ten percent", BoundaryStats{TotalWrites: 1000, Blocked: 100}, "C+"},
		{"noisy", BoundaryStats{TotalWrites: 100, Blocked: 30}, "C-"},
	} {
		if g := GradeBoundary(tc.in); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
}

func TestSummarizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		in   BoundaryStats
		want string
	}{
		{BoundaryStats{}, "No agent writes to GitHub recorded in this window."},
		{BoundaryStats{TotalWrites: 4210, Blocked: 3}, "4210 agent writes to GitHub; 3 were refused by the proxy (0.07%). No sentinel hits."},
		{BoundaryStats{TotalWrites: 10, Blocked: 1, SentinelHits: 1}, "10 agent writes to GitHub; 1 was refused by the proxy (10.00%). 1 sentinel hit."},
	} {
		if got := SummarizeBoundary(tc.in); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeCIGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   CIGateStats
		want string
	}{
		{"too few", CIGateStats{MergedN: 30, WithChecks: 19}, NoGradeLetter},
		{"clean", CIGateStats{MergedN: 60, WithChecks: 58}, "A+"},
		{"one in two hundred", CIGateStats{MergedN: 200, WithChecks: 200, Breaches: 2}, "C+"},
		{"three percent", CIGateStats{MergedN: 100, WithChecks: 100, Breaches: 3}, "C"},
		{"worse", CIGateStats{MergedN: 100, WithChecks: 100, Breaches: 4}, "C-"},
	} {
		if g := GradeCIGate(tc.in); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
}

func TestSummarizeCIGate(t *testing.T) {
	for _, tc := range []struct {
		in   CIGateStats
		want string
	}{
		{CIGateStats{}, "No PRs merged in this window."},
		{CIGateStats{MergedN: 5}, "5 PRs merged; none has a known check state at merge yet."},
		{CIGateStats{MergedN: 60, WithChecks: 58}, "All 58 merged PRs with a known state had green required checks."},
		{CIGateStats{MergedN: 60, WithChecks: 58, AgentSettled: 40, MedianPasses: fptr(1)}, "All 58 merged PRs with a known state had green required checks. Agent PRs needed a median 1 CI pass."},
		{CIGateStats{MergedN: 60, WithChecks: 58, Breaches: 2, AgentSettled: 40, MedianPasses: fptr(2.5)}, "2 of 58 merged PRs had a red or missing required check. Agent PRs needed a median 2.5 CI passes."},
		{CIGateStats{MergedN: 60, WithChecks: 58, AgentSettled: 0, MedianPasses: fptr(1)}, "All 58 merged PRs with a known state had green required checks."},
	} {
		if got := SummarizeCIGate(tc.in); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeWorkBalance(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   WorkBalanceStats
		want string
	}{
		{"too few", WorkBalanceStats{IssuesOpened: 19, IssuesClosed: 100}, NoGradeLetter},
		{"closing faster", WorkBalanceStats{IssuesOpened: 80, IssuesClosed: 100}, "A+"},
		{"balanced", WorkBalanceStats{IssuesOpened: 100, IssuesClosed: 100}, "A-"},
		{"slightly behind", WorkBalanceStats{IssuesOpened: 110, IssuesClosed: 100}, "B+"},
		{"1.5", WorkBalanceStats{IssuesOpened: 150, IssuesClosed: 100}, "B-"},
		{"2.0", WorkBalanceStats{IssuesOpened: 200, IssuesClosed: 100}, "C"},
		{"nothing closed", WorkBalanceStats{IssuesOpened: 40}, "C-"},
	} {
		if g := GradeWorkBalance(tc.in); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
	if r := GradeWorkBalance(WorkBalanceStats{IssuesOpened: 40}).Reason; !strings.Contains(r, "∞") {
		t.Errorf("nothing-closed reason should show an infinite ratio: %q", r)
	}
}

func TestSummarizeWorkBalance(t *testing.T) {
	for _, tc := range []struct {
		in   WorkBalanceStats
		want string
	}{
		{WorkBalanceStats{}, "No issues or PRs opened, closed or merged in this window."},
		{WorkBalanceStats{IssuesOpened: 120, IssuesClosed: 130, PRsOpened: 90, PRsMerged: 80}, "120 issues opened, 130 closed. 90 PRs opened, 80 merged. The issue queue is shrinking."},
		{WorkBalanceStats{IssuesOpened: 130, IssuesClosed: 120, PRsOpened: 90, PRsMerged: 80}, "130 issues opened, 120 closed. 90 PRs opened, 80 merged. The issue queue is growing."},
		{WorkBalanceStats{IssuesOpened: 10, IssuesClosed: 10}, "10 issues opened, 10 closed. 0 PRs opened, 0 merged. The issue queue is holding steady."},
		{WorkBalanceStats{PRsOpened: 3, PRsMerged: 1}, "0 issues opened, 0 closed. 3 PRs opened, 1 merged."},
	} {
		if got := SummarizeWorkBalance(tc.in); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

func TestGradeShadow(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ShadowStats
		want string
	}{
		{"too few", ShadowStats{N: 19, Held: 0}, NoGradeLetter},
		{"quiet", ShadowStats{N: 100, Held: 5}, "A+"},
		{"ten percent", ShadowStats{N: 100, Held: 10}, "A"},
		{"twenty", ShadowStats{N: 100, Held: 20}, "B+"},
		{"forty", ShadowStats{N: 100, Held: 40}, "B-"},
		{"half", ShadowStats{N: 100, Held: 50}, "C+"},
		{"most", ShadowStats{N: 100, Held: 90}, "C-"},
	} {
		if g := GradeShadow(tc.in); g.Letter != tc.want {
			t.Errorf("%s: letter %q, want %q (reason %q)", tc.name, g.Letter, tc.want, g.Reason)
		}
	}
}

func TestSummarizeShadow(t *testing.T) {
	for _, tc := range []struct {
		in   ShadowStats
		want string
	}{
		{ShadowStats{}, "No shadow-gate observations in this window; nothing was held or blocked."},
		{ShadowStats{N: 60, Held: 4}, "The shadow gates would have held 4 of 60 PRs; nothing was actually blocked."},
	} {
		if got := SummarizeShadow(tc.in); got != tc.want || words(got) > PlainSummaryMaxWords {
			t.Errorf("got %q (%d words), want %q", got, words(got), tc.want)
		}
	}
}

// TestNoGradeShape pins the "—" grade the dashboard renders grey.
func TestNoGradeShape(t *testing.T) {
	g := NoGrade("why", "how")
	if g.Letter != NoGradeLetter || g.Band != BandNone || g.EnoughData || g.Reason != "why" || g.Rubric != "how" {
		t.Errorf("unexpected NoGrade %+v", g)
	}
}

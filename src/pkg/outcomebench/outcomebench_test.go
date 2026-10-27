package outcomebench

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// bundle returns a minimal live bundle for repo o/r opened at t0+offset.
func bundle(number int, head string, offset time.Duration) *evidence.Bundle {
	at := t0.Add(offset)
	return &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, ID: evidence.BundleID("o/r", number, head),
		Repo: "o/r", Number: number, Author: evidence.Author{Login: "a", Kind: evidence.AuthorAgent},
		BaseSHA: "base", HeadSHA: head, CreatedAt: at, UpdatedAt: at,
	}
}

func verdict(v string) evidence.Verdict {
	return evidence.Verdict{Perspective: "p", Model: "m", Backend: "b", Verdict: v, Confidence: 0.9, RecordedAt: t0}
}

func merged(b *evidence.Bundle, ttm time.Duration) *evidence.Bundle {
	b.Merge = &evidence.MergeEvent{Actor: "hive", Method: "squash", At: b.CreatedAt.Add(ttm), SHA: b.HeadSHA, OpenedAt: b.CreatedAt}
	return b
}

func TestClassifyCohorts(t *testing.T) {
	// #1: two heads, verdict only on the older one — still mediated.
	old := bundle(1, "aaaaaaa1", 0)
	old.Verdicts = []evidence.Verdict{verdict("Changes_Requested")}
	newer := merged(bundle(1, "bbbbbbb1", time.Hour), 2*time.Hour)
	newer.CI.Status = CIStatusFailure
	// #2: posted review only — mediated, no changes requested.
	b2 := bundle(2, "ccccccc2", 0)
	b2.PostedReviews = []evidence.PostedReview{{URL: "u", Head: "ccccccc2", At: t0}}
	// #3: nothing — unmediated and reverted; merge.sha is a merge commit no
	// head matches, so CI is read from the newest record (pending).
	b3 := merged(bundle(3, "ddddddd3", 0), time.Hour)
	b3.Merge.SHA = "9999999abc"
	b3.RevertedBy = []string{"o/r#9@x"}
	b3.CI.Status = "pending"
	// #4: unmediated, merged, reopened issue inside the link window, CI
	// captured on a different (older) head that matches merge.sha.
	b4old := bundle(4, "eeeeeee4", 0)
	b4old.CI.Status = CIStatusSuccess
	b4 := bundle(4, "fffffff4", time.Hour)
	b4.CI.Status = CIStatusFailure
	b4.Merge = &evidence.MergeEvent{Actor: "x", Method: "merge", At: t0.Add(3 * time.Hour), SHA: "eeeeeee4"}
	b4.IssueReopens = []evidence.IssueReopen{{Issue: 5, At: t0.Add(4 * time.Hour), Source: "s"}}
	// #5: other repo, filtered out by Repo.
	b5 := bundle(5, "1111111", 0)
	b5.Repo, b5.ID = "x/y", evidence.BundleID("x/y", 5, "1111111")
	// #6: opened before Since.
	b6 := bundle(6, "2222222", -48*time.Hour)

	rows := Classify([]*evidence.Bundle{newer, old, b2, nil, b3, b4old, b4, b5, b6}, Options{Repo: "O/R", Since: t0.Add(-time.Hour), Until: t0.Add(24 * time.Hour)})
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %+v", len(rows), rows)
	}
	want := []struct {
		key                      string
		mediated, cr, rev, cfail bool
		ci                       string
	}{
		{"o/r#1", true, true, false, false, CIStatusFailure},
		{"o/r#2", true, false, false, false, ""},
		{"o/r#3", false, false, true, true, ""},
		{"o/r#4", false, false, false, true, CIStatusSuccess},
	}
	for i, w := range want {
		r := rows[i]
		if r.Key != w.key || r.Mediated != w.mediated || r.ChangesRequested != w.cr || r.Reverted != w.rev || r.ChangeFailure != w.cfail || r.CIAtMerge != w.ci {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
	}
	if rows[1].Merged() || !rows[0].Merged() {
		t.Fatalf("merged flags wrong: %+v", rows)
	}

	// Until is exclusive.
	if got := Classify([]*evidence.Bundle{b2}, Options{Until: t0}); len(got) != 0 {
		t.Fatalf("until must be exclusive, got %+v", got)
	}
}

func TestWilson(t *testing.T) {
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Fatalf("n=0: %v %v", lo, hi)
	}
	lo, hi := Wilson(5, 10)
	if math.Abs(lo-0.2366) > 1e-3 || math.Abs(hi-0.7634) > 1e-3 {
		t.Fatalf("5/10: %v %v", lo, hi)
	}
	if lo, hi := Wilson(0, 20); lo > 1e-9 || math.Abs(hi-0.1611) > 1e-3 {
		t.Fatalf("0/20: %v %v", lo, hi)
	}
	if lo, hi := Wilson(20, 20); hi < 1-1e-9 || hi > 1 || math.Abs(lo-0.8389) > 1e-3 {
		t.Fatalf("20/20: %v %v", lo, hi)
	}
	if (Rate{}).Value() != 0 || (Rate{K: 1, N: 4}).Value() != 0.25 {
		t.Fatal("Rate.Value")
	}
}

func TestSuppressionBelowMinN(t *testing.T) {
	if got := FormatRate(Rate{K: 1, N: MinN - 1}); got != "not reported (n=9)" {
		t.Fatalf("got %q", got)
	}
	if got := FormatRate(Rate{K: 5, N: MinN}); got != "50.0% (95% CI 23.7–76.3%; 5/10)" {
		t.Fatalf("got %q", got)
	}
	if got := FormatMedian(Median{D: time.Hour, N: 3}); got != "not reported (n=3)" {
		t.Fatalf("got %q", got)
	}
	if got := FormatMedian(Median{D: 90 * time.Minute, N: MinN}); got != "1.5h (n=10)" {
		t.Fatalf("got %q", got)
	}
}

func TestAggregateMedianTTMAndOutcomes(t *testing.T) {
	var rows []PRRow
	// 11 merged: TTMs 1h..11h → median 6h. One lacks opened_at and is skipped.
	for i := 1; i <= 11; i++ {
		m := &evidence.MergeEvent{At: t0.Add(time.Duration(i) * time.Hour), OpenedAt: t0}
		rows = append(rows, PRRow{Mediated: true, Merge: m, OutcomeKnown: true, CIAtMerge: CIStatusSuccess})
	}
	rows = append(rows,
		PRRow{Mediated: true, Merge: &evidence.MergeEvent{At: t0}, OutcomeKnown: true, Reverted: true, ChangeFailure: true, CIAtMerge: CIStatusFailure, ChangesRequested: true, Backfilled: true},
		PRRow{Closed: true, OutcomeKnown: true},
		PRRow{OutcomeKnown: true},
		PRRow{},
	)
	s := Aggregate("x", rows)
	if s.PRs != 15 || s.Merged != 12 || s.Closed != 1 || s.Open != 1 || s.Unknown != 1 || s.Backfilled != 1 || s.MergedMediated != 12 {
		t.Fatalf("population = %+v", s)
	}
	if s.TimeToMerge.N != 11 || s.TimeToMerge.D != 6*time.Hour {
		t.Fatalf("median = %+v", s.TimeToMerge)
	}
	if s.Revert != (Rate{1, 12}) || s.ChangeFailure != (Rate{1, 12}) || s.CIFailure != (Rate{1, 12}) || s.ChangesRequested != (Rate{1, 12}) {
		t.Fatalf("rates = %+v", s)
	}
	// Even count: mean of the middle two.
	even := Aggregate("e", rows[:10])
	if even.TimeToMerge.D != 5*time.Hour+30*time.Minute {
		t.Fatalf("even median = %v", even.TimeToMerge.D)
	}
}

// fleet builds n merged PRs (mediated when asked) from number base, every third
// one requesting changes and reverted, plus the given control window.
func fleet(base, n int, mediated bool, arm string) []*evidence.Bundle {
	var out []*evidence.Bundle
	for i := range n {
		num := base + i
		b := merged(bundle(num, "head000"+strconv.Itoa(num), time.Duration(i)*time.Minute), time.Duration(i+1)*time.Hour)
		b.CI.Status = CIStatusSuccess
		b.ControlWindow = arm
		if mediated {
			b.Verdicts = []evidence.Verdict{verdict("approve")}
		}
		if i%3 == 0 {
			b.RevertedBy = []string{"o/r#1@x"}
			if mediated {
				b.Verdicts = append(b.Verdicts, verdict(VerdictChangesRequested))
			}
		}
		out = append(out, b)
	}
	return out
}

func TestBuildAndRender(t *testing.T) {
	bundles := append(fleet(100, 12, true, evidence.ControlWindowOn), fleet(200, 12, false, evidence.ControlWindowOff)...)
	bundles = append(bundles, fleet(300, 3, false, "")...)
	spend := 120.0
	opts := Options{Root: "/data/evidence", Since: t0.Add(-time.Hour), Until: t0.Add(30 * 24 * time.Hour), FleetSpendUSD: &spend}
	r := Build(bundles, 2, opts)
	if r.PRs != 27 || r.Mediated.PRs != 12 || r.Unmediated.PRs != 15 || !r.HasControlWindow || r.ControlOn.PRs != 12 || r.ControlOff.PRs != 12 {
		t.Fatalf("report = %+v", r)
	}
	if usd, ok, rep := r.CostPerMergedPR(); !ok || !rep || usd != 10 {
		t.Fatalf("cost = %v %v %v", usd, ok, rep)
	}
	md := Render(r)
	for _, want := range []string{
		"# Outcome benchmark: Hive-mediated vs unmediated pull requests",
		"- **Evidence root:** `/data/evidence`",
		"- **Repository:** all repositories",
		"- **Window:** PRs opened from 2026-10-01T11:00:00Z to 2026-10-31T12:00:00Z (exclusive)",
		"- **PRs in window:** 27 (12 mediated, 15 unmediated; 0 backfilled from forge history)",
		"- **Files skipped as unreadable or not schema v1:** 2",
		"| Metric | mediated | unmediated |",
		"| Revert rate (merged PRs) | 33.3% (95% CI ",
		"; 4/12) | 33.3% (95% CI ",
		"; 5/15) |",
		"| Hive `changes_requested` verdict rate (mediated PRs) | 33.3% (95% CI ",
		"n/a (no Hive verdicts by definition) |",
		"| Median time to merge | 6.5h (n=12) | ",
		"| Cost per merged PR | $10.00 ($120.00 ÷ 12 merged mediated PRs) | n/a (no Hive spend by definition) |",
		"| Metric | control_window=on | control_window=off |",
		"## Not available in evidence schema v1",
		"Post-merge CI re-runs",
		"Human `changes_requested` reviews",
		"## Methodology",
		"n < 10 reads `not reported`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("render missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "--fleet-spend-usd") {
		t.Error("spend supplied, yet listed as not available")
	}
	if again := Render(Build(bundles, 2, opts)); again != md {
		t.Fatal("render is not deterministic")
	}
}

func TestRenderSmallAndEmpty(t *testing.T) {
	// Too few PRs: every rate cell is suppressed; no control window; no spend.
	r := Build(fleet(1, 3, true, ""), 0, Options{Repo: "o/r"})
	md := Render(r)
	for _, want := range []string{
		"`(not recorded)`",
		"- **Repository:** o/r",
		"from the first record to the last record",
		"| Revert rate (merged PRs) | not reported (n=3) | not reported (n=0) |",
		"| Cost per merged PR | not available (no fleet spend supplied) |",
		"No PR in the window carries a `control_window` assignment",
		"`--fleet-spend-usd`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("render missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "95% CI") {
		t.Errorf("suppressed report must not carry intervals:\n%s", md)
	}

	spend := 5.0
	r = Build(fleet(1, 3, true, ""), 0, Options{FleetSpendUSD: &spend})
	if _, ok, rep := r.CostPerMergedPR(); !ok || rep {
		t.Fatal("cost with n<10 must be supplied but not reported")
	}
	if md := Render(r); !strings.Contains(md, "| Cost per merged PR | not reported (n=3) |") {
		t.Errorf("cost cell:\n%s", md)
	}
}

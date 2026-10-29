package outcomebench

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

var now = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)

func TestWilsonMatchesKnownValues(t *testing.T) {
	lo, hi := Wilson(0, 41)
	if lo != 0 || math.Abs(hi-0.0859) > 0.001 {
		t.Fatalf("Wilson(0,41) = [%f, %f]", lo, hi)
	}
	lo, hi = Wilson(5, 10)
	if math.Abs(lo-0.2366) > 0.001 || math.Abs(hi-0.7634) > 0.001 {
		t.Fatalf("Wilson(5,10) = [%f, %f]", lo, hi)
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Fatalf("Wilson(0,0) = [%f, %f]", lo, hi)
	}
}

func TestRateAndMedianRefuseSmallN(t *testing.T) {
	if r := NewRate(3, MinN-1); r.Reported || r.Rate != 0 {
		t.Fatalf("n<MinN must be unreported: %+v", r)
	}
	if r := NewRate(3, MinN); !r.Reported || r.Rate != 0.3 || r.Lo <= 0 || r.Hi >= 1 {
		t.Fatalf("n=MinN must be reported with interval: %+v", r)
	}
	if m := NewMedian([]float64{1, 2, 3}); m.Reported {
		t.Fatalf("median below MinN reported: %+v", m)
	}
	xs := []float64{9, 1, 8, 2, 7, 3, 6, 4, 5, 10}
	if m := NewMedian(xs); !m.Reported || m.Value != 6 || xs[0] != 9 {
		t.Fatalf("median = %+v (input mutated: %v)", m, xs)
	}
	if !strings.Contains(pct(NewRate(1, 2)), NotReported) || !strings.Contains(hours(NewMedian(nil)), NotReported) {
		t.Fatal("renderers must print not reported below MinN")
	}
}

// bundle builds a merged v1 bundle; verdict "" means unmediated.
func bundle(repo string, n int, merged time.Time, ttm time.Duration, verdict string, kind string) *evidence.Bundle {
	b := &evidence.Bundle{SchemaVersion: evidence.SchemaVersion, ID: repo + "-" + time.Duration(n).String(), Repo: repo, Number: n, HeadSHA: "h", CreatedAt: merged}
	b.Author.Kind = kind
	b.Merge = &evidence.MergeEvent{At: merged, OpenedAt: merged.Add(-ttm), SHA: "m"}
	if verdict != "" {
		b.Verdicts = []evidence.Verdict{{Perspective: "p", Verdict: verdict, RecordedAt: merged}}
		b.CI.Status = "success"
	}
	return b
}

func fixture() []*evidence.Bundle {
	var bs []*evidence.Bundle
	oct := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 12; i++ {
		v := "approve"
		if i%3 == 0 {
			v = "changes_requested"
		}
		bs = append(bs, bundle("o/a", i, oct.Add(time.Duration(i)*time.Hour), 30*time.Minute, v, "agent"))
	}
	for i := 100; i < 112; i++ {
		b := bundle("o/b", i, oct.Add(time.Duration(i)*time.Hour), 10*time.Minute, "", "human")
		if i == 100 {
			b.RevertedBy = []string{"o/b#200"}
		}
		if i == 101 {
			b.FollowedUpBy = []string{"o/b#201"}
		}
		bs = append(bs, b)
	}
	// An earlier head with a changes_requested verdict on PR 1 and a merge
	// too young to have matured; both must be folded correctly.
	early := bundle("o/a", 1, oct.Add(time.Hour), 30*time.Minute, "changes_requested", "agent")
	early.Merge, early.CreatedAt, early.HeadSHA = nil, oct, "h0"
	early.Verdicts[0].RecordedAt = oct
	bs = append(bs, early)
	bs = append(bs, bundle("o/a", 50, now.Add(-time.Hour), time.Hour, "approve", "agent"))
	// Ten open PRs sent back on a first head and approved on a second head
	// 20 minutes later, for the follow-up metrics.
	for i := 400; i < 410; i++ {
		at := oct.Add(time.Duration(i) * time.Minute)
		first := bundle("o/a", i, at, time.Hour, "changes_requested", "agent")
		first.Merge, first.HeadSHA, first.CI.Status = nil, "h0", "failure"
		second := bundle("o/a", i, at.Add(20*time.Minute), time.Hour, "approve", "agent")
		second.Merge = nil
		bs = append(bs, first, second)
	}
	// September history for the trend and a PR outside the window.
	for i := 300; i < 311; i++ {
		bs = append(bs, bundle("o/a", i, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Minute), time.Hour, "", "bot"))
	}
	return bs
}

func TestComputeCohorts(t *testing.T) {
	since, until, err := ParseWindow("2026-10")
	if err != nil {
		t.Fatal(err)
	}
	rep := Compute(fixture(), 2, Options{Since: since, Until: until, Now: now, Source: "fixture",
		Spend: &Spend{USD: 26, MergedPRs: 13}})
	m, u := rep.Mediated, rep.Unmediated
	if m.Merged != 13 || u.Merged != 12 || rep.PRs != 46 || len(rep.Repos) != 2 || rep.Repos[0].Repo != "o/a" {
		t.Fatalf("cohorts: med %d unmed %d prs %d repos %+v", m.Merged, u.Merged, rep.PRs, rep.Repos)
	}
	if m.Matured != 12 || m.RevertRate.K != 0 || !m.RevertRate.Reported {
		t.Fatalf("mediated maturation: %+v", m)
	}
	if u.RevertRate.K != 1 || u.ChangeFailure.K != 2 || u.ChangeFailure.N != 12 {
		t.Fatalf("unmediated links: revert %+v change-failure %+v", u.RevertRate, u.ChangeFailure)
	}
	if m.TimeToMerge.Value < 0.49 || m.TimeToMerge.Value > 1.01 || math.Abs(u.TimeToMerge.Value-1.0/6) > 1e-9 {
		t.Fatalf("ttm: %+v / %+v", m.TimeToMerge, u.TimeToMerge)
	}
	// PRs 3,6,9,12 plus PR 1 via its earlier head.
	if m.ChangesRequested.K != 5 || m.ChangesRequested.N != 13 {
		t.Fatalf("changes requested: %+v", m.ChangesRequested)
	}
	if !m.CIFailureAtHead.Reported || m.CIFailureAtHead.K != 0 || u.CIFailureAtHead.Reported || u.CICoverage.K != 0 {
		t.Fatalf("ci: %+v / %+v", m.CIFailureAtHead, u.CIFailureAtHead)
	}
	if m.ByAuthorKind["agent"] != 13 || u.ByAuthorKind["human"] != 12 || rep.Verdicts != 34 {
		t.Fatalf("kinds %v %v verdicts %d", m.ByAuthorKind, u.ByAuthorKind, rep.Verdicts)
	}
	if rep.FirstMediatedMerge != oct(1) {
		t.Fatalf("first mediated merge %s", rep.FirstMediatedMerge)
	}
	if strings.Join(rep.Months, ",") != "2026-09,2026-10" || rep.Trend[CohortUnmediated][0].Merged != 11 || rep.Trend[CohortMediated][1].Merged != 13 {
		t.Fatalf("trend: %v %+v", rep.Months, rep.Trend)
	}
	if !rep.SplitSupported() || rep.Month() != "2026-10" || rep.AssetBase() != "outcomes-2026-10" {
		t.Fatalf("split/month: %v %s", rep.SplitSupported(), rep.Month())
	}
	if per, ok := rep.Spend.PerMergedPR(); !ok || per != 2 {
		t.Fatalf("per merged PR %f %v", per, ok)
	}
	md := rep.Markdown()
	for _, want := range []string{"# Outcome benchmark — 2026-10", "| Merged PRs in window | 13 | 12 |", "## Methodology", "≈$2.00 est.", "o/a 13, o/b 12", "not consulted", "assets/outcomes-2026-10-bars.svg", "flagged `changes_requested`"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	if strings.Contains(md, "does not support a clean") {
		t.Error("split must be reported as supported")
	}
	for _, want := range []string{"Revert rates are indistinguishable", "Median time-to-merge", "not comparable", "per hive-attributed merged PR"} {
		if !strings.Contains(strings.Join(rep.Findings(), "\n"), want) {
			t.Errorf("findings missing %q: %v", want, rep.Findings())
		}
	}
}

func oct(h int) time.Time { return time.Date(2026, 10, 2, h, 0, 0, 0, time.UTC) }

func TestComputeRefusesUnsupportedSplit(t *testing.T) {
	bs := fixture()[:3]
	rep := Compute(bs, 0, Options{Now: now})
	if rep.SplitSupported() {
		t.Fatal("3 PRs cannot support a split")
	}
	md := rep.Markdown()
	if !strings.Contains(md, "does not support a clean mediated-vs-unmediated split") || !strings.Contains(md, NotReported) {
		t.Fatalf("markdown must say so:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Findings(), ""), "nothing is claimed") {
		t.Fatalf("findings: %v", rep.Findings())
	}
	block := rep.ReadmeBlock("src/docs/benchmarks")
	if !strings.Contains(block, "Not enough reviewed pull requests are on record yet") || strings.Contains(block, "card-accuracy") || strings.Contains(block, "card-also") {
		t.Fatalf("readme block must not claim a comparison or draw a card below the minimum base:\n%s", block)
	}
	for _, r := range []Rate{rep.Mediated.RevertRate, rep.Unmediated.TimeToMergeRate()} {
		if r.Reported {
			t.Fatalf("reported below MinN: %+v", r)
		}
	}
}

// TimeToMergeRate adapts the median to the Rate check above.
func (c Cohort) TimeToMergeRate() Rate {
	return Rate{N: c.TimeToMerge.N, Reported: c.TimeToMerge.Reported}
}

func TestSVGsAreWellFormedAndHonest(t *testing.T) {
	since, until, _ := ParseWindow("2026-10")
	rep := Compute(fixture(), 0, Options{Since: since, Until: until, Now: now})
	bars, trend := rep.BarChartSVG(), rep.TrendSVG()
	for _, svg := range []string{bars, trend} {
		if !strings.HasPrefix(svg, "<svg xmlns=") || !strings.HasSuffix(svg, "</svg>\n") || strings.Count(svg, "<text") < 8 {
			t.Fatalf("malformed svg:\n%s", svg)
		}
		if !strings.Contains(svg, colourMediated) || !strings.Contains(svg, colourUnmediated) {
			t.Fatal("svg must use both series colours")
		}
	}
	// Unmediated CI has no captured status: it is a hatched placeholder, and
	// the changes-requested group has no unmediated bar at all.
	if !strings.Contains(bars, `stroke-dasharray="3 3"`) || strings.Count(bars, "(n=") < 6 {
		t.Fatalf("bars must label n and hatch unreported cells:\n%s", bars)
	}
	if !strings.Contains(trend, ">2026-09<") || !strings.Contains(trend, ">2026-10<") || !strings.Contains(trend, "<path d=\"M") {
		t.Fatalf("trend must plot both months with a line:\n%s", trend)
	}
	if niceMax(0.07) != 0.1 || niceMax(2) != 1 || niceHours(0.3) != 0.5 || niceHours(500) != 504 {
		t.Fatal("axis rounding")
	}
	empty := Compute(nil, 0, Options{Now: now})
	if s := empty.TrendSVG(); !strings.HasSuffix(s, "</svg>\n") {
		t.Fatal("empty trend must still be a document")
	}
}

func TestReadmeBlockRoundTrip(t *testing.T) {
	since, until, _ := ParseWindow("2026-10")
	rep := Compute(fixture(), 0, Options{Since: since, Until: until, Now: now, Spend: &Spend{USD: 26, MergedPRs: 13}, ReadmeMinBase: 1,
		Stories: []Story{{Repo: "hivecommons/hive", Number: 1, Caught: "A test was skipped.", OpenedAt: now.Add(-20 * time.Minute), CaughtAt: now.Add(-15 * time.Minute), FixApprovedAt: now.Add(-10 * time.Minute), MergedAt: now}},
		RedTeam: &RedTeam{Attacks: 10, Caught: 9, Benign: 5, FalseAlarms: 1, RunAt: now}})
	block := rep.ReadmeBlock("src/docs/benchmarks")
	for _, want := range []string{ReadmeStart, ReadmeEnd, "## Accuracy", "## Security", "## Cost", "## And the rest", "46 live pull requests in 2 repositories", "(oct '26)", "Humans still merge", "open PRs only", "The review agent sent 15 pull requests back. Three out of four came back fixed and approved, usually within 20 minutes.", "We planted 10 texts", "Hive caught 9 before an agent read a word.", "One harmless text in 5 got flagged by mistake.", "Call it $2 a merge, our estimate (", "Every pull request gets the same one-line review", "src/docs/benchmarks/outcomes-2026-10.md",
		"card-accuracy-dark.png", "card-accuracy-light.png", "card-stories-dark.png", "card-prompt-injection-light.png", "card-cost-dark.png", "card-also-light.png", "prefers-color-scheme: dark", "How we count"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	for _, banned := range []string{"Wilson", "interval", "(n=", " n=", "median", "mediated", "95%", "≈", ".svg", ";", "Numbers below"} {
		if strings.Contains(block, banned) {
			t.Errorf("block contains %q; statistical vocabulary belongs in the report", banned)
		}
	}
	if n := strings.Count(block, "\n"); n > 60 {
		t.Fatalf("block is %d lines; README additions must stay small", n+1)
	}
	readme := "# Hive\n\nintro\n\n" + ReadmeStart + "\nstale\n" + ReadmeEnd + "\n\n## Next\n"
	updated, err := UpdateReadme(readme, block)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "stale") || !strings.HasPrefix(updated, "# Hive\n\nintro\n\n"+ReadmeStart) || !strings.HasSuffix(updated, ReadmeEnd+"\n\n## Next\n") {
		t.Fatalf("update:\n%s", updated)
	}
	got, err := ExtractReadmeBlock(updated)
	if err != nil || got != block {
		t.Fatalf("extract mismatch (%v):\n%s", err, got)
	}
	if _, err := UpdateReadme("no markers", block); err != ErrNoMarkers {
		t.Fatalf("expected ErrNoMarkers, got %v", err)
	}
	if _, err := ExtractReadmeBlock(ReadmeEnd + ReadmeStart); err != ErrNoMarkers {
		t.Fatalf("reversed markers: %v", err)
	}
}

func TestLoadAndLoaders(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, v any) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(v)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("o/a/1/h.json", bundle("o/a", 1, now, time.Hour, "approve", "agent"))
	write("o/a/2/h.json", map[string]any{"schema_version": "v0", "repo": "o/a", "number": 2})
	if err := os.WriteFile(filepath.Join(dir, "o/a/junk.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "o/a/notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bs, unreadable, err := Load(dir)
	if err != nil || len(bs) != 1 || unreadable != 2 {
		t.Fatalf("Load = %d bundles, %d unreadable, %v", len(bs), unreadable, err)
	}
	if bs, _, err := Load(filepath.Join(dir, "missing")); err != nil || len(bs) != 0 {
		t.Fatalf("missing root must be empty: %v", err)
	}

	write("cost.json", map[string]any{"estimated": map[string]any{"total_usd": 6124.75}, "merged_prs": 2869, "count_window_start": 1786147200000, "count_window_end": 1793112641723, "count_basis": "hive-attributed"})
	sp, err := LoadSpend(filepath.Join(dir, "cost.json"))
	if err != nil || sp.MergedPRs != 2869 || sp.Basis != "hive-attributed" || sp.WindowStart.Format("2006-01-02") != "2026-08-08" {
		t.Fatalf("LoadSpend = %+v, %v", sp, err)
	}
	if per, ok := sp.PerMergedPR(); !ok || math.Abs(per-2.135) > 0.001 {
		t.Fatalf("per merged PR %f", per)
	}
	write("minimal.json", map[string]any{"usd": 10, "merged_prs": 5})
	small, err := LoadSpend(filepath.Join(dir, "minimal.json"))
	if err != nil || small.USD != 10 {
		t.Fatalf("minimal spend: %+v %v", small, err)
	}
	if _, ok := small.PerMergedPR(); ok {
		t.Fatal("5 merged PRs is below MinN")
	}
	write("empty.json", map[string]any{})
	if _, err := LoadSpend(filepath.Join(dir, "empty.json")); err == nil {
		t.Fatal("empty spend must fail")
	}
	if _, err := LoadSpend(filepath.Join(dir, "o/a/junk.json")); err == nil {
		t.Fatal("junk spend must fail")
	}
	if _, err := LoadSpend(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing spend must fail")
	}

	write("hub.json", map[string]any{"spokes": 7, "reporting": 5, "window": map[string]string{"from": "2026-07-29", "to": "2026-10-27"}})
	hub, err := LoadHub(filepath.Join(dir, "hub.json"))
	if err != nil || hub.Spokes != 7 || hub.Window.From != "2026-07-29" {
		t.Fatalf("LoadHub = %+v, %v", hub, err)
	}
	rep := Compute(nil, 0, Options{Now: now, Hub: hub})
	if !strings.Contains(rep.Markdown(), "7 spokes registered, 5 reporting") {
		t.Fatal("hub context must be quoted")
	}
	if _, err := LoadHub(filepath.Join(dir, "empty.json")); err == nil {
		t.Fatal("empty hub must fail")
	}
	if _, err := LoadHub(filepath.Join(dir, "o/a/junk.json")); err == nil {
		t.Fatal("junk hub must fail")
	}
	if _, err := LoadHub(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing hub must fail")
	}
	if _, _, err := ParseWindow("Oct 2026"); err == nil {
		t.Fatal("bad month must fail")
	}
}

func TestOpenWindowUsesGenerationMonth(t *testing.T) {
	rep := Compute(nil, 0, Options{Now: now})
	if rep.Month() != "2026-10" || rep.GeneratedAt != now {
		t.Fatalf("month %s generated %s", rep.Month(), rep.GeneratedAt)
	}
	if !strings.Contains(rep.Markdown(), "Window: merged open → open") {
		t.Fatal("open window must be stated")
	}
}

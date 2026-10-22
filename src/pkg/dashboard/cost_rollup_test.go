package dashboard

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

func rollupSnap(t time.Time, usd float64, in, out int64) CostHistoryEntry {
	return CostHistoryEntry{
		Timestamp: t.UnixMilli(),
		USD:       usd,
		Models:    map[string]CostModelSnap{"m": {Input: in, Output: out, USD: usd}},
	}
}

// Folding attributes each positive increment to the UTC day of the snapshot
// that revealed it, carries the baseline across midnight, ignores counter
// drops, and drops a one-off re-seed jump that beats both discontinuity tests.
func TestCostFoldDailyRollups(t *testing.T) {
	day1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var hist []CostHistoryEntry
	// Day 1: $1 every hour from 00:00 → 23:00 (23 increments).
	for h := 0; h < 24; h++ {
		hist = append(hist, rollupSnap(day1.Add(time.Duration(h)*time.Hour), float64(h), int64(h*1000), int64(h*10)))
	}
	// Day 2: first snapshot at 00:00 is +1 over day 1's last ($23 → $24): that
	// dollar belongs to day 2. Then a counter DROP (sessions aged out) to $5
	// which must not go negative, then +2.
	day2 := day1.AddDate(0, 0, 1)
	hist = append(hist,
		rollupSnap(day2, 24, 24000, 240),
		rollupSnap(day2.Add(time.Hour), 5, 5000, 50),
		rollupSnap(day2.Add(2*time.Hour), 7, 7000, 70),
	)
	// Day 3: a $5,000 restore in one step — a data correction, not spend —
	// followed by an ordinary +1.
	day3 := day2.AddDate(0, 0, 1)
	hist = append(hist,
		rollupSnap(day3, 5007, 7000, 70),
		rollupSnap(day3.Add(time.Hour), 5008, 8000, 80),
	)

	got := costFoldDailyRollups(hist)
	if len(got) != 3 {
		t.Fatalf("days = %d, want 3: %+v", len(got), got)
	}
	d1 := got["2026-09-01"]
	if d1.USD != 23 || d1.Samples != 24 || d1.InputTokens != 23000 || d1.OutputTokens != 230 {
		t.Errorf("day 1 = %+v, want $23 / 24 samples / 23000 in / 230 out", d1)
	}
	d2 := got["2026-09-02"]
	if d2.USD != 3 || d2.Samples != 3 || d2.InputTokens != 3000 || d2.OutputTokens != 30 {
		t.Errorf("day 2 = %+v, want $3 (1 carried over midnight + 2 after the drop) / 3 samples", d2)
	}
	d3 := got["2026-09-03"]
	if d3.USD != 1 || d3.Samples != 2 {
		t.Errorf("day 3 = %+v, want $1 (the $5000 restore is discarded) / 2 samples", d3)
	}
}

func TestCostRollupDiscontinuityCeiling(t *testing.T) {
	if c := costRollupDiscontinuityCeiling([]float64{1}); !math.IsInf(c, 1) {
		t.Errorf("one increment → no ceiling, got %v", c)
	}
	// Median $1 → 20× = $20, below the $500 floor → floor wins.
	if c := costRollupDiscontinuityCeiling([]float64{1, 1, 1, 5000}); c != costRollupDiscontinuityFloorUSD {
		t.Errorf("ceiling = %v, want the $%d floor", c, costRollupDiscontinuityFloorUSD)
	}
	// Median $40 → 20× = $800 beats the floor.
	if c := costRollupDiscontinuityCeiling([]float64{30, 40, 50}); c != 800 {
		t.Errorf("ceiling = %v, want 800", c)
	}
}

// The live fold stores only complete UTC days, never overwrites a stored day
// once the snapshot ring has slid past its midnight, prunes beyond the
// retention horizon, and surfaces the rollup on /api/cost.
func TestCostRollupFoldLifecycle(t *testing.T) {
	s := covCServer(t)
	// SeedCostDailyRollups folds with the wall clock, so "today" must be the
	// real UTC date: a fixed date turns into a complete day the moment the
	// calendar moves past it, and the still-open day below gets stored.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12*time.Hour + 30*time.Minute)
	today := now.Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	older := today.AddDate(0, 0, -50).Format(costRollupDayLayout)

	var hist []CostHistoryEntry
	for h := 0; h < 24; h++ {
		hist = append(hist, rollupSnap(yesterday.Add(time.Duration(h)*time.Hour), float64(h), 0, 0))
	}
	hist = append(hist, rollupSnap(today, 24, 0, 0), rollupSnap(today.Add(time.Hour), 25, 0, 0))
	s.costSeries().Seed(hist)
	s.foldCostRollups(now)

	got := s.CostDailyRollups()
	if len(got) != 1 || got[0].Day != yesterday.Format(costRollupDayLayout) || got[0].USD != 23 {
		t.Fatalf("rollups after first fold = %+v, want only yesterday at $23 (today is still open)", got)
	}

	// The ring slides past yesterday's midnight: the fold now sees a partial
	// yesterday ($5 of increments) but must keep the stored full-day $23.
	s.costSeries().Seed(hist[18:])
	s.foldCostRollups(now)
	got = s.CostDailyRollups()
	if len(got) != 1 || got[0].USD != 23 {
		t.Fatalf("partial-day refold overwrote the stored total: %+v", got)
	}

	// Seeded history is honoured, malformed days are dropped, and days beyond
	// the retention horizon are pruned.
	ancient := now.AddDate(0, 0, -costDailyRollupRetentionDays-1).Format(costRollupDayLayout)
	s.SeedCostDailyRollups([]CostDailyRollup{
		{Day: older, USD: 12, Samples: 288},
		{Day: ancient, USD: 99, Samples: 288},
		{Day: "not-a-day", USD: 1, Samples: 1},
	})
	s.foldCostRollups(now)
	got = s.CostDailyRollups()
	if len(got) != 2 || got[0].Day != older || got[1].Day != yesterday.Format(costRollupDayLayout) || got[1].USD != 23 {
		t.Fatalf("rollups after seed = %+v, want %s then yesterday", got, older)
	}

	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/cost = %d", rec.Code)
	}
	var body struct {
		DailyRollup []CostDailyRollup `json:"daily_rollup"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/cost: %v", err)
	}
	if len(body.DailyRollup) != 2 || body.DailyRollup[0].USD != 12 {
		t.Errorf("/api/cost daily_rollup = %+v, want the two stored days", body.DailyRollup)
	}
}

// AppendCostHistoryFull folds after each stored snapshot so the rollup never
// lags the ring by more than one sample.
func TestAppendCostHistoryFoldsRollup(t *testing.T) {
	s := &Server{}
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	s.costSeries().Seed([]CostHistoryEntry{
		rollupSnap(yesterday, 1, 0, 0),
		rollupSnap(yesterday.Add(time.Hour), 4, 0, 0),
	})
	s.AppendCostHistoryFull(6, nil, nil)
	got := s.CostDailyRollups()
	if len(got) != 1 || got[0].USD != 3 {
		t.Fatalf("rollup after append = %+v, want yesterday at $3", got)
	}
}

// A sampler gap (the collector only ran while a browser was polling, #13483)
// reveals days of real spend in one step. That step must be kept when a
// plausible rate over the elapsed time explains it, while a same-size jump
// at normal cadence is still a correction (#13482).
func TestCostFoldDailyRollupsKeepsGapExplainedSteps(t *testing.T) {
	day1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var hist []CostHistoryEntry
	// $40 every 5 minutes for 24 samples: median step $40 → $800 fixed
	// ceiling. Ordinary spend is $920 over the 48h10m span → ≈$19/h average,
	// ×20 ≈ $382/h plausible rate.
	for i := 0; i < 24; i++ {
		hist = append(hist, rollupSnap(day1.Add(time.Duration(i)*5*time.Minute), float64(40*i), int64(1000*i), int64(10*i)))
	}
	last := hist[len(hist)-1]
	// 48 h of silence, then +$10,000: above the $800 fixed ceiling, but
	// 48 h × $382/h ≈ $18,300 of plausible accrual — real spend.
	gapEnd := day1.Add(48 * time.Hour)
	hist = append(hist, rollupSnap(gapEnd, last.USD+10000, 1_000_000, 10_000))
	// Five minutes later another +$10,000: nothing can accrue that in 5 min
	// ($382/h × 5 min ≈ $32) — correction, and its tokens go with it.
	hist = append(hist, rollupSnap(gapEnd.Add(5*time.Minute), last.USD+20000, 900_000_000, 10_000))
	// Then an ordinary +$40.
	hist = append(hist, rollupSnap(gapEnd.Add(10*time.Minute), last.USD+20040, 900_001_000, 10_010))

	got := costFoldDailyRollups(hist)
	d3 := got["2026-09-03"]
	if d3.USD != 10040 {
		t.Errorf("gap day USD = %v, want 10040 (gap-explained $10,000 kept, instant $10,000 dropped, +$40 kept)", d3.USD)
	}
	if d3.InputTokens != 1_000_000-int64(last.Models["m"].Input)+1000 {
		t.Errorf("gap day input tokens = %d, want the kept steps only (no 899M from the discarded step)", d3.InputTokens)
	}
}

func TestCostRollupStepAllowed(t *testing.T) {
	rate := 20 * (480.0 / float64(time.Hour/time.Millisecond)) // 20 × $480/h in $/ms
	if !costRollupStepAllowed(500, 1, 800, rate) {
		t.Error("a step under the fixed ceiling is always spend")
	}
	if costRollupStepAllowed(10000, int64(5*time.Minute/time.Millisecond), 800, rate) {
		t.Error("$10,000 in 5 minutes is a correction")
	}
	if !costRollupStepAllowed(10000, int64(48*time.Hour/time.Millisecond), 800, rate) {
		t.Error("$10,000 over 48 h is explained by the rate")
	}
	if !costRollupStepAllowed(10000, 0, 800, math.Inf(1)) {
		t.Error("with no rate baseline nothing is discarded by rate")
	}
	if costRollupStepAllowed(10000, 0, 800, rate) {
		t.Error("a zero gap cannot explain a step above the ceiling")
	}
	if r := costRollupDiscontinuityRate([]costRollupStep{{USD: 1, GapMs: 1}}, 100); !math.IsInf(r, 1) {
		t.Errorf("one step → no rate baseline, got %v", r)
	}
	if r := costRollupDiscontinuityRate([]costRollupStep{{USD: 10, GapMs: 10}, {USD: 20, GapMs: 10}}, 0); !math.IsInf(r, 1) {
		t.Errorf("no span → no rate baseline, got %v", r)
	}
	// $60 of ordinary spend over a 30 ms span → 2 $/ms average; the $5,000
	// outlier is excluded from the baseline it is judged against.
	if r := costRollupDiscontinuityRate([]costRollupStep{{USD: 10, GapMs: 10}, {USD: 20, GapMs: 10}, {USD: 30, GapMs: 10}, {USD: 5000, GapMs: 10}}, 30); r != 20*2 {
		t.Errorf("rate = %v, want 20 × average 2 $/ms", r)
	}
}

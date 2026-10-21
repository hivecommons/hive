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
	now := time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC)
	today := now.Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)

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
		{Day: "2026-09-01", USD: 12, Samples: 288},
		{Day: ancient, USD: 99, Samples: 288},
		{Day: "not-a-day", USD: 1, Samples: 1},
	})
	s.foldCostRollups(now)
	got = s.CostDailyRollups()
	if len(got) != 2 || got[0].Day != "2026-09-01" || got[1].USD != 23 {
		t.Fatalf("rollups after seed = %+v, want 2026-09-01 then yesterday", got)
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
	yesterday := time.Now().UTC().Truncate(24 * time.Hour).AddDate(0, 0, -1)
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

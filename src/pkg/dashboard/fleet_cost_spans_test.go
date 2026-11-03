package dashboard

import (
	"math"
	"testing"
	"time"
)

func TestFleetCostWindowStarts(t *testing.T) {
	// Sunday 2026-11-01 13:00 UTC: ISO week began Monday 2026-10-26, the
	// month began that same day at 00:00, the day at 00:00 Nov 1.
	now := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)
	day, week, month := fleetCostWindowStarts(now)
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !day.Equal(want) {
		t.Fatalf("day start %v, want %v", day, want)
	}
	if want := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC); !week.Equal(want) {
		t.Fatalf("week start %v, want %v", week, want)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !month.Equal(want) {
		t.Fatalf("month start %v, want %v", month, want)
	}
	// A Monday in a non-UTC zone still resolves to the UTC Monday.
	mon := time.Date(2026, 10, 26, 1, 0, 0, 0, time.FixedZone("plus3", 3*3600)) // Sunday 22:00 UTC
	_, week, _ = fleetCostWindowStarts(mon)
	if want := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC); !week.Equal(want) {
		t.Fatalf("week start across zone %v, want %v", week, want)
	}
}

func TestFleetCostWindowsSumsLedgerAndLiveTail(t *testing.T) {
	now := time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC) // Sunday; week = Oct 26..Nov 1
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	// Ring: a snapshot late on Oct 31 at $100, then two today at $104 and $110.
	hist := []CostHistoryEntry{
		{Timestamp: ms(time.Date(2026, 10, 31, 23, 50, 0, 0, time.UTC)), USD: 100},
		{Timestamp: ms(time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC)), USD: 104},
		{Timestamp: ms(time.Date(2026, 11, 1, 12, 55, 0, 0, time.UTC)), USD: 110},
	}
	// Stored complete days: three in the ISO week, one before it, and one
	// from the previous month inside the week (Oct 26..31).
	rollups := []CostDailyRollup{
		{Day: "2026-10-20", USD: 50, Samples: 10},
		{Day: "2026-10-26", USD: 7, Samples: 10},
		{Day: "2026-10-30", USD: 3, Samples: 10},
		{Day: "2026-10-31", USD: 5, Samples: 10},
		{Day: "2026-11-01", USD: 999, Samples: 10}, // today: incomplete, must be ignored
	}
	f := FrontendFleetRun{Running: 3, HourUSD: 2, HourPriced: true}
	got := fleetCostWindows(f, hist, rollups, 111.5, true, now)
	if !got.WindowsPriced {
		t.Fatal("windows should be priced with a ledger present")
	}
	// Today from the fold: (104-100)+(110-104)=10, live tail 111.5-110=1.5.
	if want := 11.5; math.Abs(got.DayUSD-want) > 1e-9 {
		t.Fatalf("day %.2f want %.2f", got.DayUSD, want)
	}
	// Week adds Oct 26, 30, 31 stored days (7+3+5); Oct 20 is outside.
	if want := 11.5 + 15; math.Abs(got.WeekUSD-want) > 1e-9 {
		t.Fatalf("week %.2f want %.2f", got.WeekUSD, want)
	}
	// Month is November only: same as today. Week > month is legitimate.
	if want := 11.5; math.Abs(got.MonthUSD-want) > 1e-9 {
		t.Fatalf("month %.2f want %.2f", got.MonthUSD, want)
	}
	if got.HourUSD != 2 || got.Running != 3 {
		t.Fatal("hour figure and running count must pass through untouched")
	}
}

func TestFleetCostWindowsFloorAtHourAndUnpricedWithoutLedger(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	f := FrontendFleetRun{HourUSD: 4, HourPriced: true}
	got := fleetCostWindows(f, nil, nil, 4, true, now)
	if got.WindowsPriced || got.DayUSD != 0 {
		t.Fatalf("no ledger must leave the windows unpriced: %+v", got)
	}
	// A ring whose last snapshot predates the hour's rise: the hour figure
	// is inside every window, so no window may read below it.
	hist := []CostHistoryEntry{{Timestamp: now.Add(-2 * time.Hour).UnixMilli(), USD: 50}}
	got = fleetCostWindows(f, hist, nil, 50, true, now)
	if !got.WindowsPriced || got.DayUSD != 4 || got.WeekUSD != 4 || got.MonthUSD != 4 {
		t.Fatalf("windows should floor at the hour figure: %+v", got)
	}
}

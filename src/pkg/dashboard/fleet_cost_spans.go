package dashboard

import "time"

// Rotating cost windows on the RUNNING tile (hivecommons/hive#15614).
//
// The run meter's hour figure is the rise of the cumulative estimate since
// the top of the hour. The wider windows come from the cost ledger the Cost
// section already keeps: the per-UTC-day spend folded out of the 5-minute
// snapshot ring (costFoldDailyRollups) for days the ring still covers, the
// stored daily rollup for complete days it no longer does, plus the live
// rise since the ring's last snapshot so "today" never lags "this hour".
// All three windows start on a UTC day boundary, so day granularity is
// exact: today 00:00 UTC, the ISO week's Monday 00:00 UTC, the 1st of the
// calendar month 00:00 UTC.

// fleetCostWindowStarts returns the UTC start of the current calendar day,
// ISO week (Monday) and calendar month containing now.
func fleetCostWindowStarts(now time.Time) (day, week, month time.Time) {
	u := now.UTC()
	day = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	wd := int(u.Weekday())
	if wd == 0 {
		wd = 7 // ISO weeks start on Monday; Sunday is day 7.
	}
	week = day.AddDate(0, 0, -(wd - 1))
	month = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return day, week, month
}

// fleetCostLedgerDays merges the stored daily rollups with the days folded
// live out of the snapshot ring into one day → $ map. A stored complete day
// wins over the live fold (the ring may only partly cover it); today and
// any other day the rollup lacks come from the fold.
func fleetCostLedgerDays(hist []CostHistoryEntry, rollups []CostDailyRollup, now time.Time) map[string]float64 {
	today := now.UTC().Format(costRollupDayLayout)
	days := map[string]float64{}
	for day, r := range costFoldDailyRollups(hist) {
		days[day] = r.USD
	}
	for _, r := range rollups {
		if r.Samples > 0 && r.Day < today {
			days[r.Day] = r.USD
		}
	}
	return days
}

// fleetCostWindowSum sums the ledger days from start (inclusive, a UTC day
// boundary) through today.
func fleetCostWindowSum(days map[string]float64, start, now time.Time) float64 {
	today := now.UTC().Format(costRollupDayLayout)
	var sum float64
	for d := start; ; d = d.AddDate(0, 0, 1) {
		key := d.Format(costRollupDayLayout)
		if key > today {
			break
		}
		sum += days[key]
	}
	return sum
}

// fleetCostWindows fills f's day/week/month figures from the ledger and the
// live cumulative total. tail is the rise since the ring's last snapshot
// (the spend the ledger has not folded yet); each window is also floored at
// the hour figure, since the hour is inside every window.
func fleetCostWindows(f FrontendFleetRun, hist []CostHistoryEntry, rollups []CostDailyRollup, liveTotal float64, liveSeen bool, now time.Time) FrontendFleetRun {
	if len(hist) == 0 && len(rollups) == 0 {
		return f
	}
	var tail float64
	if liveSeen && len(hist) > 0 && liveTotal > hist[len(hist)-1].USD {
		tail = liveTotal - hist[len(hist)-1].USD
	}
	days := fleetCostLedgerDays(hist, rollups, now)
	dayStart, weekStart, monthStart := fleetCostWindowStarts(now)
	window := func(start time.Time) float64 {
		v := fleetCostWindowSum(days, start, now) + tail
		if f.HourPriced && v < f.HourUSD {
			v = f.HourUSD
		}
		return v
	}
	f.DayUSD = window(dayStart)
	f.WeekUSD = window(weekStart)
	f.MonthUSD = window(monthStart)
	f.WindowsPriced = true
	return f
}

// attachFleetCostWindows is the Server-side step: the status builders are
// free functions without the ledger, so the windows are added when the
// snapshot is published.
func (s *Server) attachFleetCostWindows(status *StatusPayload, now time.Time) {
	if status == nil {
		return
	}
	total, seen := defaultRunMeter.latestTotal()
	status.FleetRun = fleetCostWindows(status.FleetRun, s.CostHistory(), s.CostDailyRollups(), total, seen, now)
}

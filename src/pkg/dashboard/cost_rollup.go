package dashboard

import (
	"math"
	"sort"
	"time"
)

// CostDailyRollup is one UTC calendar day's estimated spend, folded out of the
// 5-minute cumulative cost snapshots before the ring buffer drops them. The
// snapshot ring keeps ~30 days; the rollup keeps costDailyRollupRetentionDays
// so the Cost section's weekly/monthly windows can reach back years with one
// tiny record per day instead of 288 snapshots.
type CostDailyRollup struct {
	// Day is the UTC calendar day, YYYY-MM-DD.
	Day string `json:"day"`
	// USD is the estimated $ spent during the day: the sum of the positive
	// step-to-step increments of the cumulative estimated total observed that
	// day (the same positive-increment rule the UI applies to the snapshots).
	USD float64 `json:"cost_usd"`
	// InputTokens / OutputTokens are the positive per-model token increments
	// summed over the day. Zero on days whose snapshots predate per-model data.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// Samples is how many snapshots fed the day; the UI treats 0 as "no data".
	Samples int `json:"samples"`
}

// costDailyRollupRetentionDays keeps daily rollups for at least two years
// (two leap-safe years); each record is ~100 bytes so the file stays small.
const costDailyRollupRetentionDays = 2 * 366

// costRollupDayLayout is the Day field's format.
const costRollupDayLayout = "2006-01-02"

// costRollupDiscontinuityMult / costRollupDiscontinuityFloorUSD mirror the
// UI's COST_DISCONTINUITY_MULT / COST_DISCONTINUITY_FLOOR_USD: a single
// snapshot-to-snapshot increase is a data correction (re-seed, restore), not
// spend, only when it beats BOTH the relative and the absolute test AND has
// the shape of a restore — it regains ground the counter had already reached
// (costRollupStepRegainsGround). A step to a new high is spend however large
// (#13497).
const (
	costRollupDiscontinuityMult     = 20
	costRollupDiscontinuityFloorUSD = 500
)

// costRollupDay is the UTC day a snapshot timestamp (unix millis) belongs to.
func costRollupDay(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(costRollupDayLayout)
}

// costRollupDayStart is the unix-millis start of a YYYY-MM-DD UTC day; ok is
// false for a malformed day string.
func costRollupDayStart(day string) (int64, bool) {
	t, err := time.ParseInLocation(costRollupDayLayout, day, time.UTC)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// costRollupDiscontinuityCeiling returns the $ above which a single positive
// restore-shaped increment (costRollupStepRegainsGround) is discarded as a
// correction: max(mult × median of the ordinary increments, floor). Steps to
// a new high are never subject to it. +Inf when there are too few ordinary
// increments to judge.
func costRollupDiscontinuityCeiling(incs []float64) float64 {
	median, ok := costRollupMedian(incs)
	if !ok {
		return math.Inf(1)
	}
	ceiling := costRollupDiscontinuityMult * median
	if ceiling < costRollupDiscontinuityFloorUSD {
		ceiling = costRollupDiscontinuityFloorUSD
	}
	return ceiling
}

// costRollupMedian is the median of the ordinary (below-floor) values; ok is
// false when fewer than two ordinary values exist or the median is not > 0.
func costRollupMedian(vals []float64) (float64, bool) {
	normal := make([]float64, 0, len(vals))
	for _, v := range vals {
		if v < costRollupDiscontinuityFloorUSD {
			normal = append(normal, v)
		}
	}
	if len(normal) < 2 {
		return 0, false
	}
	sort.Float64s(normal)
	mid := len(normal) / 2
	median := normal[mid]
	if len(normal)%2 == 0 {
		median = (normal[mid-1] + normal[mid]) / 2
	}
	return median, median > 0
}

// costRollupStep is one positive step of the cumulative total together with
// the time the sampler took to observe it.
type costRollupStep struct {
	USD   float64
	GapMs int64
}

// costRollupDiscontinuityRate returns the plausible-spend rate ceiling in
// $/ms: mult × the hive's average rate, where the average is the ordinary
// (below-floor) spend divided by the span the history covers. The fixed
// ceiling above judges a step as if it arrived at the normal 5-minute
// cadence, but the sampler is view-driven and a hive that nobody looked at
// for a week reveals a week of real spend in one step. That step is spend,
// not a correction, whenever it is explained by elapsed time × a plausible
// rate (hivecommons/hive#13482). The average — not the median — is the
// baseline because spend arrives in lumps (a session's tokens appear when
// its transcript flushes) between long runs of near-zero steps, so the
// median step rate says nothing about how fast the hive really spends.
// +Inf when there are too few ordinary steps or no span to divide by.
func costRollupDiscontinuityRate(steps []costRollupStep, spanMs int64) float64 {
	var sum float64
	n := 0
	for _, st := range steps {
		if st.USD < costRollupDiscontinuityFloorUSD && st.GapMs > 0 {
			sum += st.USD
			n++
		}
	}
	if n < 2 || spanMs <= 0 || !(sum > 0) {
		return math.Inf(1)
	}
	return costRollupDiscontinuityMult * sum / float64(spanMs)
}

// costRollupStepAllowed reports whether a positive step observed over gapMs
// is spend: it passes the fixed per-step ceiling, or it is within what the
// plausible rate could have accrued over the gap.
func costRollupStepAllowed(usd float64, gapMs int64, ceiling, ratePerMs float64) bool {
	if usd <= ceiling {
		return true
	}
	if gapMs <= 0 || math.IsInf(ratePerMs, 1) {
		return math.IsInf(ratePerMs, 1)
	}
	return usd <= ratePerMs*float64(gapMs)
}

// costRollupRestoreGapFraction mirrors the UI's COST_RESTORE_GAP_FRACTION.
const costRollupRestoreGapFraction = 0.8

// costRollupStepRegainsGround reports whether a positive step lands at or
// below a level the counter already reached and closes most of the gap to it
// — the shape of a restore. A step to a new high is spend, however large.
func costRollupStepRegainsGround(inc, prev, cur, peak float64) bool {
	if math.IsInf(peak, -1) || cur > peak {
		return false
	}
	return inc >= costRollupRestoreGapFraction*(peak-prev)
}

// costFoldDailyRollups folds a cumulative snapshot series into per-UTC-day
// spend records. Increments are attributed to the day of the snapshot that
// revealed them, so a day's first increment counts the growth since the last
// snapshot of the previous day. Entries must be in timestamp order (the ring
// buffer guarantees it).
func costFoldDailyRollups(hist []CostHistoryEntry) map[string]CostDailyRollup {
	out := map[string]CostDailyRollup{}
	if len(hist) == 0 {
		return out
	}
	incs := make([]float64, 0, len(hist))
	steps := make([]costRollupStep, 0, len(hist))
	for i := 1; i < len(hist); i++ {
		if d := hist[i].USD - hist[i-1].USD; d > 0 {
			incs = append(incs, d)
			steps = append(steps, costRollupStep{USD: d, GapMs: hist[i].Timestamp - hist[i-1].Timestamp})
		}
	}
	ceiling := costRollupDiscontinuityCeiling(incs)
	rate := costRollupDiscontinuityRate(steps, hist[len(hist)-1].Timestamp-hist[0].Timestamp)

	var prev *CostHistoryEntry
	peak := math.Inf(-1)
	for i := range hist {
		e := &hist[i]
		day := costRollupDay(e.Timestamp)
		r := out[day]
		r.Day = day
		r.Samples++
		if prev != nil {
			d := e.USD - prev.USD
			// A step discarded as a correction takes its token deltas with
			// it: counting the tokens of a re-seed while dropping its dollars
			// made 800M-token / $0 days (#13482).
			// Only a restore (regained ground) can be a correction; a step to a
			// new high is a session flush however large (#13497).
			if d > 0 && costRollupStepRegainsGround(d, prev.USD, e.USD, peak) && !costRollupStepAllowed(d, e.Timestamp-prev.Timestamp, ceiling, rate) {
				// A regained-ground step is at or below peak by definition,
				// so peak is unchanged here.
				out[day] = r
				prev = e
				continue
			}
			if d > 0 {
				r.USD += d
			}
			for name, cur := range e.Models {
				p, ok := prev.Models[name]
				if !ok {
					continue
				}
				if d := cur.Input - p.Input; d > 0 {
					r.InputTokens += d
				}
				if d := cur.Output - p.Output; d > 0 {
					r.OutputTokens += d
				}
			}
		}
		out[day] = r
		if e.USD > peak {
			peak = e.USD
		}
		prev = e
	}
	return out
}

// foldCostRollups refreshes the daily rollup from the current snapshot ring.
// Only days that are complete (before today, UTC) are stored, and a day already
// stored is recomputed only while the ring still reaches back to its start —
// once the ring has slid past a day's midnight the stored total is final, so
// an eroding window can never overwrite a full day with a partial one. Days
// beyond the retention horizon are pruned.
func (s *Server) foldCostRollups(now time.Time) {
	hist := s.CostHistory()
	folded := costFoldDailyRollups(hist)
	today := now.UTC().Format(costRollupDayLayout)
	horizon := now.UTC().AddDate(0, 0, -costDailyRollupRetentionDays).Format(costRollupDayLayout)
	var firstTS int64
	if len(hist) > 0 {
		firstTS = hist[0].Timestamp
	}

	s.costRollupMu.Lock()
	defer s.costRollupMu.Unlock()
	if s.costRollup == nil {
		s.costRollup = map[string]CostDailyRollup{}
	}
	for day, r := range folded {
		if day >= today || day < horizon {
			continue
		}
		if _, have := s.costRollup[day]; have {
			start, ok := costRollupDayStart(day)
			if !ok || firstTS > start {
				continue
			}
		}
		s.costRollup[day] = r
	}
	for day := range s.costRollup {
		if day < horizon {
			delete(s.costRollup, day)
		}
	}
}

// CostWindowStart is the instant the persisted cost record begins: the start
// of the oldest daily rollup when there is one, else the oldest snapshot still
// in the ring. The rollup is the stable anchor — the ring evicts its oldest
// sample on every append once full, so a window anchored on the ring creeps
// forward hour by hour (#13481). Zero when there is no cost data at all.
func (s *Server) CostWindowStart() time.Time {
	var oldest int64
	for _, r := range s.CostDailyRollups() {
		if start, ok := costRollupDayStart(r.Day); ok && r.Samples > 0 {
			oldest = start
			break
		}
	}
	if hist := s.CostHistory(); len(hist) > 0 && hist[0].Timestamp > 0 && (oldest == 0 || hist[0].Timestamp < oldest) {
		oldest = hist[0].Timestamp
	}
	if oldest == 0 {
		return time.Time{}
	}
	return time.UnixMilli(oldest).UTC()
}

// CostDailyRollups returns the stored daily rollups in ascending day order.
func (s *Server) CostDailyRollups() []CostDailyRollup {
	s.costRollupMu.Lock()
	defer s.costRollupMu.Unlock()
	out := make([]CostDailyRollup, 0, len(s.costRollup))
	for _, r := range s.costRollup {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// SeedCostDailyRollups restores persisted rollups on startup. Malformed days
// are dropped; the live fold then fills in whatever the snapshot ring covers.
func (s *Server) SeedCostDailyRollups(entries []CostDailyRollup) {
	s.costRollupMu.Lock()
	if s.costRollup == nil {
		s.costRollup = map[string]CostDailyRollup{}
	}
	for _, r := range entries {
		if _, ok := costRollupDayStart(r.Day); !ok {
			continue
		}
		s.costRollup[r.Day] = r
	}
	s.costRollupMu.Unlock()
	s.foldCostRollups(time.Now())
}

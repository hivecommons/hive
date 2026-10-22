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
// snapshot-to-snapshot increase is a data correction (re-seed, restore, price
// change), not spend, only when it beats BOTH the relative and the absolute
// test AND is not explained by the step's own per-model token increments (see
// costRollupStepTokenBacked). A large step backed by token growth is real
// spend that accrued across a sampling gap.
const (
	costRollupDiscontinuityMult     = 20
	costRollupDiscontinuityFloorUSD = 500
)

// costRollupMedianMinIncUSD mirrors the UI's COST_DISCONTINUITY_MIN_INC_USD:
// sub-cent increments are excluded from the median so a quiet fleet (cents per
// step) cannot collapse the baseline.
const costRollupMedianMinIncUSD = 0.01

// costRollupTokenBackedRatio is the share of a step's $ increase that must be
// accounted for by the $ of models whose token counters grew in that step for
// the step to count as token-backed spend.
const costRollupTokenBackedRatio = 0.5

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
// increment is discarded as a correction: max(mult × median of the ordinary
// increments, floor). Sub-cent increments do not count as ordinary. +Inf when
// there are too few ordinary increments to judge.
func costRollupDiscontinuityCeiling(incs []float64) float64 {
	normal := make([]float64, 0, len(incs))
	for _, v := range incs {
		if v >= costRollupMedianMinIncUSD && v < costRollupDiscontinuityFloorUSD {
			normal = append(normal, v)
		}
	}
	if len(normal) < 2 {
		return math.Inf(1)
	}
	sort.Float64s(normal)
	mid := len(normal) / 2
	median := normal[mid]
	if len(normal)%2 == 0 {
		median = (normal[mid-1] + normal[mid]) / 2
	}
	if !(median > 0) {
		return math.Inf(1)
	}
	ceiling := costRollupDiscontinuityMult * median
	if ceiling < costRollupDiscontinuityFloorUSD {
		ceiling = costRollupDiscontinuityFloorUSD
	}
	return ceiling
}

// costRollupStepTokenBacked reports whether a positive $ step d from prev to
// cur is explained by per-model token growth: the $ increase of the models
// whose input or output counters grew in the step covers at least
// costRollupTokenBackedRatio of d. Steps without per-model data on both sides
// (legacy snapshots) are never token-backed.
func costRollupStepTokenBacked(prev, cur *CostHistoryEntry, d float64) bool {
	if len(prev.Models) == 0 || len(cur.Models) == 0 || !(d > 0) {
		return false
	}
	var backed float64
	for name, c := range cur.Models {
		p, ok := prev.Models[name]
		if !ok || (c.Input <= p.Input && c.Output <= p.Output) {
			continue
		}
		if du := c.USD - p.USD; du > 0 {
			backed += du
		}
	}
	return backed >= costRollupTokenBackedRatio*d
}

// costFoldDailyRollups folds a cumulative snapshot series into per-UTC-day
// spend records. Increments are attributed to the day of the snapshot that
// revealed them, so a day's first increment counts the growth since the last
// snapshot of the previous day. Entries must be in timestamp order (the ring
// buffer guarantees it).
//
// A step above the discontinuity ceiling is discarded as a data correction
// only when it is not token-backed (costRollupStepTokenBacked); a discarded
// step's token increments are discarded with it, so a day's cost_usd and
// *_tokens always describe the same steps.
func costFoldDailyRollups(hist []CostHistoryEntry) map[string]CostDailyRollup {
	out := map[string]CostDailyRollup{}
	if len(hist) == 0 {
		return out
	}
	incs := make([]float64, 0, len(hist))
	for i := 1; i < len(hist); i++ {
		if d := hist[i].USD - hist[i-1].USD; d > 0 {
			incs = append(incs, d)
		}
	}
	ceiling := costRollupDiscontinuityCeiling(incs)

	var prev *CostHistoryEntry
	for i := range hist {
		e := &hist[i]
		day := costRollupDay(e.Timestamp)
		r := out[day]
		r.Day = day
		r.Samples++
		if prev != nil {
			d := e.USD - prev.USD
			if d > ceiling && !costRollupStepTokenBacked(prev, e, d) {
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
		prev = e
	}
	return out
}

// foldCostRollups refreshes the daily rollup from the current snapshot ring.
// Only days that are complete (before today, UTC) are stored, and a day already
// stored is recomputed only while the ring still reaches back to its start —
// once the ring has slid past a day's midnight the stored total can only grow:
// the partial refold replaces it only when it recovers more spend (e.g. a day
// folded before token-backed gap steps were kept), so an eroding window can
// never overwrite a full day with a smaller partial one. Days beyond the
// retention horizon are pruned.
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
		if stored, have := s.costRollup[day]; have {
			start, ok := costRollupDayStart(day)
			if !ok || (firstTS > start && r.USD <= stored.USD) {
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

// costAllTime is the headline all-time estimate: the persisted daily rollups
// plus the ring's folded days that are not stored yet (today's partial), so it
// neither double counts lineage residuals nor shrinks as the 30-day ring
// evicts. since is the start of the oldest rollup day (stable), else the first
// snapshot; with no history at all it falls back to the live estimate.
func (s *Server) costAllTime(est costEstimated) (usd float64, since int64) {
	rollups := s.CostDailyRollups()
	hist := s.CostHistory()
	if len(rollups) == 0 && len(hist) == 0 {
		return est.TotalUSD, est.WindowStart
	}
	stored := make(map[string]bool, len(rollups))
	for _, r := range rollups {
		stored[r.Day] = true
		usd += r.USD
	}
	for day, r := range costFoldDailyRollups(hist) {
		if !stored[day] {
			usd += r.USD
		}
	}
	if len(rollups) > 0 {
		since, _ = costRollupDayStart(rollups[0].Day)
	} else {
		since = hist[0].Timestamp
	}
	return usd, since
}

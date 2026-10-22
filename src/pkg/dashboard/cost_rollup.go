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
// test AND is explained neither by elapsed time × a plausible rate
// (costRollupStepAllowed) nor by the step's own per-model token increments
// (costRollupStepTokenBacked). A large step revealed after a sampling gap or
// backed by token growth (e.g. a daily session flush) is real spend.
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

// costRollupStepTokenBacked reports whether a positive $ step d from prev to
// cur is explained by per-model token growth: the $ increase of the models
// whose input or output counters grew in the step covers at least
// costRollupTokenBackedRatio of d. A model absent from prev counts from zero:
// the token summary lists only sessions still on disk, so a daily session
// flush often brings back a model that had aged out of the previous snapshot.
// Steps without per-model data on both sides (legacy snapshots) are never
// token-backed.
func costRollupStepTokenBacked(prev, cur *CostHistoryEntry, d float64) bool {
	if len(prev.Models) == 0 || len(cur.Models) == 0 || !(d > 0) {
		return false
	}
	var backed float64
	for name, c := range cur.Models {
		p := prev.Models[name]
		if c.Input <= p.Input && c.Output <= p.Output {
			continue
		}
		if du := c.USD - p.USD; du > 0 {
			backed += du
		}
	}
	return backed >= costRollupTokenBackedRatio*d
}

// costRollupMedian is the median of the ordinary values — at least
// costRollupMedianMinIncUSD and below the floor; ok is false when fewer than
// two ordinary values exist or the median is not > 0.
func costRollupMedian(vals []float64) (float64, bool) {
	normal := make([]float64, 0, len(vals))
	for _, v := range vals {
		if v >= costRollupMedianMinIncUSD && v < costRollupDiscontinuityFloorUSD {
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

// costFoldDailyRollups folds a cumulative snapshot series into per-UTC-day
// spend records. Increments are attributed to the day of the snapshot that
// revealed them, so a day's first increment counts the growth since the last
// snapshot of the previous day. Entries must be in timestamp order (the ring
// buffer guarantees it).
//
// A step above the discontinuity ceiling is discarded as a data correction
// only when the gap-aware rate rule does not allow it (costRollupStepAllowed)
// and it is not token-backed (costRollupStepTokenBacked); a discarded
// step's token increments are discarded with it, so a day's cost_usd and
// *_tokens always describe the same steps.
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
	for i := range hist {
		e := &hist[i]
		day := costRollupDay(e.Timestamp)
		r := out[day]
		r.Day = day
		r.Samples++
		if prev != nil {
			d := e.USD - prev.USD
			// A step above the ceiling is spend when elapsed time × a plausible
			// rate explains it (sampling gap) OR the step's own per-model token
			// growth explains it (e.g. a daily session flush, #13497). A step
			// discarded as a correction takes its token deltas with it:
			// counting the tokens of a re-seed while dropping its dollars made
			// 800M-token / $0 days (#13482).
			if d > 0 && !costRollupStepAllowed(d, e.Timestamp-prev.Timestamp, ceiling, rate) && !costRollupStepTokenBacked(prev, e, d) {
				out[day] = r
				prev = e
				continue
			}
			if d > 0 {
				r.USD += d
			}
			for name, cur := range e.Models {
				p, ok := prev.Models[name]
				if !ok && len(prev.Models) == 0 {
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

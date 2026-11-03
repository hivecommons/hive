package collect

import (
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// launchCostRollupRetentionDays bounds the stored per-day series.
const launchCostRollupRetentionDays = 2 * 366

const launchCostRollupDayLayout = "2006-01-02"

// LaunchCostRollupRef is one work ref's attributed spend on one UTC day.
type LaunchCostRollupRef struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
	// Launches counts every delivered kick that named the ref on the day,
	// including kicks that received no usage — the same repeat-spend counter
	// as /api/launch-cost by_ref[].launches.
	Launches int `json:"launches"`
	// ShareUSD splits each launch's spend evenly across the refs it named,
	// like RefCostEntry.ShareUSD. Zero when no usage landed on the day's launches.
	ShareUSD float64 `json:"share_usd"`
}

// LaunchCostDailyRollup is the per-ref launch attribution for one UTC day,
// kept beyond the /api/launch-cost accounting window so a multi-day per-PR
// series can be measured.
type LaunchCostDailyRollup struct {
	// Day is the UTC calendar day of the launches, YYYY-MM-DD.
	Day   string                `json:"day"`
	ByRef []LaunchCostRollupRef `json:"by_ref"`
}

// launchCostLiveLaunch is one launch still visible to the join. Its share is
// re-read on every tick while usage keeps landing on it; once the launch
// leaves the window or the launch ring, its last value is folded into the
// settled per-ref totals of its day exactly once.
type launchCostLiveLaunch struct {
	Day  string             `json:"day"`
	Refs []tokens.LaunchRef `json:"refs"`
	// ShareUSD is the per-ref share of the launch's spend (usd / len(refs)).
	ShareUSD float64 `json:"share_usd"`
}

// launchCostLiveKey identifies a launch the way ComputeLaunchCost does
// (agent + launch instant), rendered through the same RFC3339 string that
// LaunchCostEntry.LaunchedAt carries so ByLaunch rows and raw records match.
func launchCostLiveKey(agent, launchedAt string) string { return agent + "@" + launchedAt }

// liveLaunches lists every launch the join currently counts for by_ref, keyed
// by launch, with its per-ref share. It mirrors ComputeLaunchCost: launches
// with usage come from resp.ByLaunch; launches that named refs but received no
// usage count when launched inside the window, so the daily launches figure
// agrees with /api/launch-cost by_ref[].launches.
func liveLaunches(resp LaunchCostResponse, launches []tokens.LaunchRecord, now time.Time) map[string]launchCostLiveLaunch {
	out := map[string]launchCostLiveLaunch{}
	for _, l := range resp.ByLaunch {
		at, err := time.Parse(time.RFC3339, l.LaunchedAt)
		if err != nil || len(l.Refs) == 0 {
			continue
		}
		var share float64
		if l.USD != nil {
			share = *l.USD / float64(len(l.Refs))
		}
		out[launchCostLiveKey(l.Agent, l.LaunchedAt)] = launchCostLiveLaunch{
			Day:      at.UTC().Format(launchCostRollupDayLayout),
			Refs:     l.Refs,
			ShareUSD: share,
		}
	}
	windowStartMs := now.Add(-time.Duration(resp.WindowHours) * time.Hour).UnixMilli()
	for _, rec := range launches {
		if rec.Agent == "" || rec.LaunchedAtMs == 0 || rec.LaunchedAtMs < windowStartMs || len(rec.Refs) == 0 {
			continue
		}
		at := msToRFC3339(rec.LaunchedAtMs)
		key := launchCostLiveKey(rec.Agent, at)
		if _, ok := out[key]; ok {
			continue
		}
		out[key] = launchCostLiveLaunch{
			Day:  time.UnixMilli(rec.LaunchedAtMs).UTC().Format(launchCostRollupDayLayout),
			Refs: rec.Refs,
		}
	}
	return out
}

// addLaunchToRefs folds one launch's refs into a day's per-ref map.
func addLaunchToRefs(refs map[string]LaunchCostRollupRef, l launchCostLiveLaunch) {
	for _, r := range l.Refs {
		if r.Ref == "" {
			continue
		}
		e := refs[r.Ref]
		e.Ref = r.Ref
		if e.Kind == "" || r.Kind == tokens.LaunchRefKindPR {
			e.Kind = r.Kind
		}
		e.Launches++
		e.ShareUSD += l.ShareUSD
		refs[r.Ref] = e
	}
}

// mergeLaunchCostRollupLocked folds a snapshot into the stored days. Each
// launch is tracked individually while the join can still see it, so a
// launch's share follows usage that keeps landing on it (never shrinking when
// older usage ages past the window start). The moment a launch drops out of
// the join — the window slid past it or the launch ring turned over — its last
// share is added to its day's settled per-ref totals. Days therefore
// accumulate every launch exactly once, however quickly the ring turns over.
func (lc *LaunchCostCollector) mergeLaunchCostRollupLocked(resp LaunchCostResponse, launches []tokens.LaunchRecord, now time.Time) {
	if !resp.Ready {
		return
	}
	if lc.rollup == nil {
		lc.rollup = map[string]map[string]LaunchCostRollupRef{}
	}
	if lc.rollupLive == nil {
		lc.rollupLive = map[string]launchCostLiveLaunch{}
	}
	live := liveLaunches(resp, launches, now)
	for key, prev := range lc.rollupLive {
		cur, ok := live[key]
		if !ok {
			refs := lc.rollup[prev.Day]
			if refs == nil {
				refs = map[string]LaunchCostRollupRef{}
				lc.rollup[prev.Day] = refs
			}
			addLaunchToRefs(refs, prev)
			delete(lc.rollupLive, key)
			lc.invalidateRollupDayLocked(prev.Day)
			continue
		}
		if cur.ShareUSD < prev.ShareUSD {
			cur.ShareUSD = prev.ShareUSD
		}
		if cur.ShareUSD != prev.ShareUSD {
			lc.invalidateRollupDayLocked(prev.Day)
		}
		lc.rollupLive[key] = cur
	}
	for key, cur := range live {
		if _, ok := lc.rollupLive[key]; ok {
			continue
		}
		lc.rollupLive[key] = cur
		lc.invalidateRollupDayLocked(cur.Day)
	}
	horizon := now.UTC().AddDate(0, 0, -launchCostRollupRetentionDays).Format(launchCostRollupDayLayout)
	for day := range lc.rollup {
		if day < horizon {
			delete(lc.rollup, day)
			lc.invalidateRollupDayLocked(day)
		}
	}
	for key, l := range lc.rollupLive {
		if l.Day < horizon {
			delete(lc.rollupLive, key)
			lc.invalidateRollupDayLocked(l.Day)
		}
	}
}

func (lc *LaunchCostCollector) invalidateRollupDayLocked(day string) {
	delete(lc.rollupDayCache, day)
	delete(lc.settledDayCache, day)
}

// rollupDayLocked returns the sorted per-ref list for one day: the settled
// totals plus every live launch on that day. Results are cached per day and
// dropped by merge only for days it touched, so a tick re-sorts the one or two
// days that changed rather than the whole retention span.
func (lc *LaunchCostCollector) rollupDayLocked(day string) []LaunchCostRollupRef {
	if cached, ok := lc.rollupDayCache[day]; ok {
		return cached
	}
	refs := map[string]LaunchCostRollupRef{}
	for ref, e := range lc.rollup[day] {
		refs[ref] = e
	}
	for _, l := range lc.rollupLive {
		if l.Day == day {
			addLaunchToRefs(refs, l)
		}
	}
	out := make([]LaunchCostRollupRef, 0, len(refs))
	for _, e := range refs {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	if lc.rollupDayCache == nil {
		lc.rollupDayCache = map[string][]LaunchCostRollupRef{}
	}
	lc.rollupDayCache[day] = out
	return out
}

func (lc *LaunchCostCollector) rollupListLocked() []LaunchCostDailyRollup {
	days := map[string]bool{}
	for day := range lc.rollup {
		days[day] = true
	}
	for _, l := range lc.rollupLive {
		days[l.Day] = true
	}
	out := make([]LaunchCostDailyRollup, 0, len(days))
	for day := range days {
		out = append(out, LaunchCostDailyRollup{Day: day, ByRef: lc.rollupDayLocked(day)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// settledRollupListLocked lists only the settled per-ref totals, for the
// store; live launches are persisted separately so a restart neither drops nor
// double-counts them.
func (lc *LaunchCostCollector) settledRollupListLocked() []LaunchCostDailyRollup {
	if lc.settledDayCache == nil {
		lc.settledDayCache = map[string]LaunchCostDailyRollup{}
	}
	out := make([]LaunchCostDailyRollup, 0, len(lc.rollup))
	for day, refs := range lc.rollup {
		d, ok := lc.settledDayCache[day]
		if !ok {
			d = LaunchCostDailyRollup{Day: day, ByRef: make([]LaunchCostRollupRef, 0, len(refs))}
			for _, e := range refs {
				d.ByRef = append(d.ByRef, e)
			}
			sort.Slice(d.ByRef, func(i, j int) bool { return d.ByRef[i].Ref < d.ByRef[j].Ref })
			lc.settledDayCache[day] = d
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// DailyRollup returns the stored per-day, per-ref launch spend in ascending
// day order. Days still inside the accounting window include the launches the
// join currently sees at their latest share.
func (lc *LaunchCostCollector) DailyRollup() []LaunchCostDailyRollup {
	if lc == nil {
		return []LaunchCostDailyRollup{}
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.rollupListLocked()
}

func (lc *LaunchCostCollector) seedRollupLocked(days []LaunchCostDailyRollup, live map[string]launchCostLiveLaunch) {
	lc.rollup = map[string]map[string]LaunchCostRollupRef{}
	lc.rollupLive = map[string]launchCostLiveLaunch{}
	lc.rollupDayCache = nil
	lc.settledDayCache = nil
	for _, d := range days {
		if _, err := time.Parse(launchCostRollupDayLayout, d.Day); err != nil {
			continue
		}
		refs := map[string]LaunchCostRollupRef{}
		for _, e := range d.ByRef {
			refs[e.Ref] = e
		}
		lc.rollup[d.Day] = refs
	}
	for key, l := range live {
		if _, err := time.Parse(launchCostRollupDayLayout, l.Day); err != nil || len(l.Refs) == 0 {
			continue
		}
		lc.rollupLive[key] = l
	}
}

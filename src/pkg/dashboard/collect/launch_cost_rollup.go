package collect

import (
	"sort"
	"time"
)

// launchCostRollupRetentionDays bounds the stored per-day series.
const launchCostRollupRetentionDays = 2 * 366

const launchCostRollupDayLayout = "2006-01-02"

// LaunchCostRollupRef is one work ref's attributed spend on one UTC day.
type LaunchCostRollupRef struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	Launches int    `json:"launches"`
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

// foldLaunchCostDays groups a snapshot's launches by the UTC day they were
// launched on and splits each launch's spend evenly across its refs.
func foldLaunchCostDays(resp LaunchCostResponse) map[string]map[string]LaunchCostRollupRef {
	out := map[string]map[string]LaunchCostRollupRef{}
	for _, l := range resp.ByLaunch {
		at, err := time.Parse(time.RFC3339, l.LaunchedAt)
		if err != nil || len(l.Refs) == 0 {
			continue
		}
		day := at.UTC().Format(launchCostRollupDayLayout)
		var share float64
		if l.USD != nil {
			share = *l.USD / float64(len(l.Refs))
		}
		refs := out[day]
		if refs == nil {
			refs = map[string]LaunchCostRollupRef{}
			out[day] = refs
		}
		for _, r := range l.Refs {
			if r.Ref == "" {
				continue
			}
			e := refs[r.Ref]
			e.Ref = r.Ref
			e.Kind = string(r.Kind)
			e.Launches++
			e.ShareUSD += share
			refs[r.Ref] = e
		}
	}
	return out
}

// mergeLaunchCostRollupLocked folds a snapshot into the stored days. The
// snapshot window slides, so a day it only partly covers is merged per ref by
// taking the larger value: a day's totals only grow and never shrink when
// older launches age out of the window.
func (lc *LaunchCostCollector) mergeLaunchCostRollupLocked(resp LaunchCostResponse, now time.Time) {
	if !resp.Ready {
		return
	}
	if lc.rollup == nil {
		lc.rollup = map[string]map[string]LaunchCostRollupRef{}
	}
	for day, refs := range foldLaunchCostDays(resp) {
		stored := lc.rollup[day]
		if stored == nil {
			stored = map[string]LaunchCostRollupRef{}
			lc.rollup[day] = stored
		}
		for ref, e := range refs {
			prev := stored[ref]
			if e.Launches < prev.Launches {
				e.Launches = prev.Launches
			}
			if e.ShareUSD < prev.ShareUSD {
				e.ShareUSD = prev.ShareUSD
			}
			stored[ref] = e
		}
	}
	horizon := now.UTC().AddDate(0, 0, -launchCostRollupRetentionDays).Format(launchCostRollupDayLayout)
	for day := range lc.rollup {
		if day < horizon {
			delete(lc.rollup, day)
		}
	}
}

func launchCostRollupList(m map[string]map[string]LaunchCostRollupRef) []LaunchCostDailyRollup {
	out := make([]LaunchCostDailyRollup, 0, len(m))
	for day, refs := range m {
		d := LaunchCostDailyRollup{Day: day, ByRef: make([]LaunchCostRollupRef, 0, len(refs))}
		for _, e := range refs {
			d.ByRef = append(d.ByRef, e)
		}
		sort.Slice(d.ByRef, func(i, j int) bool { return d.ByRef[i].Ref < d.ByRef[j].Ref })
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// DailyRollup returns the stored per-day, per-ref launch spend in ascending
// day order.
func (lc *LaunchCostCollector) DailyRollup() []LaunchCostDailyRollup {
	if lc == nil {
		return []LaunchCostDailyRollup{}
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return launchCostRollupList(lc.rollup)
}

func (lc *LaunchCostCollector) seedRollupLocked(days []LaunchCostDailyRollup) {
	lc.rollup = map[string]map[string]LaunchCostRollupRef{}
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
}

package dashboard

import (
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	"github.com/hivecommons/hive/pkg/tokens"
)

// benchmarksCostLookup attributes kick spend to a benchmark record from the
// launch-cost collector: USD from the persisted daily rollup (every day the
// store has seen, live days at their latest share) and token counts from the
// in-window snapshot, each launch split evenly across the refs it named —
// the same accounting as /api/launch-cost by_ref[].share_usd. Nil collector
// means nothing is attributed.
func benchmarksCostLookup(lc *collect.LaunchCostCollector) benchmarks.CostLookup {
	if lc == nil {
		return nil
	}
	return func(refs []benchmarks.CostRef) (benchmarks.Cost, bool) {
		want := map[benchmarks.CostRef]bool{}
		for _, r := range refs {
			want[r] = true
		}
		var usd float64
		var launches int
		for _, day := range lc.DailyRollup() {
			for _, ref := range day.ByRef {
				if want[benchmarks.CostRef{Ref: ref.Ref, Kind: ref.Kind}] {
					usd += ref.ShareUSD
					launches += ref.Launches
				}
			}
		}
		if usd <= 0 {
			return benchmarks.Cost{Basis: benchmarks.CostBasisUnknown}, false
		}
		c := benchmarks.Cost{
			Basis:          benchmarks.CostBasisTokensXPricing,
			USDEst:         &usd,
			Launches:       launches,
			PriceTableDate: tokens.PriceTableDate(),
		}
		if snap, ok := lc.Snapshot(); ok {
			addLaunchTokens(&c, snap.ByLaunch, want)
		}
		return c, true
	}
}

// addLaunchTokens adds the per-ref token share of every in-window launch
// that named one of the wanted refs. Tokens are informational: usd_est is
// already priced by the collector.
func addLaunchTokens(c *benchmarks.Cost, launches []collect.LaunchCostEntry, want map[benchmarks.CostRef]bool) {
	var in, out, read, write int64
	seen := false
	for _, l := range launches {
		n := int64(len(l.Refs))
		if n == 0 {
			continue
		}
		hit := 0
		for _, r := range l.Refs {
			if want[benchmarks.CostRef{Ref: r.Ref, Kind: r.Kind}] {
				hit++
			}
		}
		if hit == 0 {
			continue
		}
		seen = true
		k := int64(hit)
		in += l.Input * k / n
		out += l.Output * k / n
		read += l.CacheRead * k / n
		write += l.CacheCreate * k / n
	}
	if seen {
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens = &in, &out, &read, &write
	}
}

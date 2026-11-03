package main

import (
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/forgestate"
)

// lateForgeOpen is a forgestate.OpenSource that resolves the state cache on
// every call. The cache is built by the first configureGitHubClient, which
// may run after the metrics collector is wired (a client first delivered
// over the heartbeat), so the counters must not capture a nil store.
type lateForgeOpen struct {
	store func() *forgestate.Store
}

func (l lateForgeOpen) Snapshot(repo string) (forgestate.Snapshot, bool) {
	if l.store == nil {
		return forgestate.Snapshot{}, false
	}
	s := l.store()
	if s == nil {
		return forgestate.Snapshot{}, false
	}
	return s.Snapshot(repo)
}

// wireForgeStateCounts moves the dashboard's merged-PR / closed-issue counts
// onto the forgestate counters (webhook increments, hourly search reconcile)
// and, when the cache is on, its Overview open totals onto the cache
// (hivecommons/hive#13376). Always on: the counters need no cache to replace
// the per-cycle search queries; without one Snapshot never reports ready and
// the Overview keeps the enumeration's totals.
func wireForgeStateCounts(mc *dashboard.MetricsCollector, store func() *forgestate.Store) *forgestate.Counts {
	if mc == nil {
		return nil
	}
	return mc.EnableForgeCounts(lateForgeOpen{store: store})
}

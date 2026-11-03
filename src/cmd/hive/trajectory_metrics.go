package main

import (
	"time"

	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/pkg/dashboard"
)

// trajectoryMetricsCounters adapts the trajectory lane's Stats to the
// dashboard's TrajectoryCounters (hivecommons/hive#12132). The dashboard
// does not import pkg/trajectory, so the translation lives here.
func trajectoryMetricsCounters(lane *trajectory.Lane) func() (dashboard.TrajectoryCounters, bool) {
	return func() (dashboard.TrajectoryCounters, bool) {
		if lane == nil {
			return dashboard.TrajectoryCounters{}, false
		}
		st := lane.Stats()
		agents := make(map[string]dashboard.TrajectoryAgentReview, len(st.Agents))
		for name, a := range st.Agents {
			last := ""
			if !a.LastReviewed.IsZero() {
				last = a.LastReviewed.UTC().Format(time.RFC3339)
			}
			agents[name] = dashboard.TrajectoryAgentReview{
				LastReviewed: last,
				LastDecision: a.LastDecision,
				LastReason:   a.LastReason,
				Skipped:      a.Skipped,
			}
		}
		return dashboard.TrajectoryCounters{
			Runs:            st.Runs,
			Reviewed:        st.Reviewed,
			Skipped:         st.Skipped,
			SkippedByReason: st.SkippedByReason,
			RanByReason:     st.RanByReason,
			Agents:          agents,
		}, true
	}
}

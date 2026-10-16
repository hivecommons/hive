package dashboard

import (
	"fmt"
	"strings"
	"sync"
)

// TrajectoryCounters are the trajectory-review lane's cumulative review/skip
// counters (hivecommons/hive#12132) exposed on /metrics and in the governor
// config trajectory section. They mirror trajectory.Stats; the dashboard
// keeps its own copy so it does not import pkg/trajectory.
type TrajectoryCounters struct {
	Runs            int                              `json:"runs"`
	Reviewed        int                              `json:"reviewed"`
	Skipped         int                              `json:"skipped"`
	SkippedByReason map[string]int                   `json:"skippedByReason"`
	RanByReason     map[string]int                   `json:"ranByReason"`
	Agents          map[string]TrajectoryAgentReview `json:"agents"`
}

// TrajectoryAgentReview is one agent's last lane decision.
type TrajectoryAgentReview struct {
	LastReviewed string `json:"lastReviewed,omitempty"`
	LastDecision string `json:"lastDecision"`
	LastReason   string `json:"lastReason"`
	Skipped      int    `json:"skipped"`
}

var (
	trajectoryCountersMu sync.RWMutex
	trajectoryCountersFn func() (TrajectoryCounters, bool)
)

// SetTrajectoryCountersProvider registers the source of the trajectory lane
// counters. fn returns ok=false when the lane is not running, in which case
// no trajectory series is written. Nil unregisters.
func SetTrajectoryCountersProvider(fn func() (TrajectoryCounters, bool)) {
	trajectoryCountersMu.Lock()
	defer trajectoryCountersMu.Unlock()
	trajectoryCountersFn = fn
}

func trajectoryCounters() (TrajectoryCounters, bool) {
	trajectoryCountersMu.RLock()
	fn := trajectoryCountersFn
	trajectoryCountersMu.RUnlock()
	if fn == nil {
		return TrajectoryCounters{}, false
	}
	return fn()
}

// writeTrajectoryMetrics appends the trajectory-review series to b.
func writeTrajectoryMetrics(b *strings.Builder, hiveID string, writeHeader func(name, help, typ string)) {
	c, ok := trajectoryCounters()
	if !ok {
		return
	}
	writeHeader("hive_trajectory_reviews_total",
		"Trajectory-review lane per-agent decisions: reviewed (sent to the reviewer model) or skipped (proven idle, not overdue).", "counter")
	fmt.Fprintf(b, "hive_trajectory_reviews_total{hive_id=%s,decision=%s} %d\n", promQuote(hiveID), promQuote("reviewed"), c.Reviewed)
	fmt.Fprintf(b, "hive_trajectory_reviews_total{hive_id=%s,decision=%s} %d\n", promQuote(hiveID), promQuote("skipped"), c.Skipped)

	writeHeader("hive_trajectory_skipped_total",
		"Trajectory reviews skipped, by reason.", "counter")
	writeReasonSeries(b, "hive_trajectory_skipped_total", hiveID, c.SkippedByReason)

	writeHeader("hive_trajectory_ran_total",
		"Trajectory reviews that ran, by the reason the skip predicate declined to skip.", "counter")
	writeReasonSeries(b, "hive_trajectory_ran_total", hiveID, c.RanByReason)
}

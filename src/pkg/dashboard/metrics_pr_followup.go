package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// PRFollowUpCounters are the cumulative PR follow-up session-resume counters
// (hivecommons/hive#9583) exposed on /metrics. They mirror prfollowup.Stats;
// the dashboard keeps its own copy so it does not import pkg/prfollowup
// (see TestDashboardInternalImportCountRatchet). The maps are keyed by
// reason, which pkg/prfollowup keeps to a small fixed set.
type PRFollowUpCounters struct {
	Resumed           int
	Fallback          map[string]int
	Skipped           map[string]int
	Deferred          int
	HandoffsQueued    int
	HandoffsDelivered int
	Pruned            map[string]int
}

var (
	prFollowUpCountersMu sync.RWMutex
	prFollowUpCountersFn func() (PRFollowUpCounters, bool)
)

// SetPRFollowUpCountersProvider registers the source of the PR follow-up
// counters. fn returns ok=false when there is nothing to report (feature off,
// or no counter file yet), in which case no pr_followup series is written.
// Nil unregisters.
func SetPRFollowUpCountersProvider(fn func() (PRFollowUpCounters, bool)) {
	prFollowUpCountersMu.Lock()
	defer prFollowUpCountersMu.Unlock()
	prFollowUpCountersFn = fn
}

func prFollowUpCounters() (PRFollowUpCounters, bool) {
	prFollowUpCountersMu.RLock()
	fn := prFollowUpCountersFn
	prFollowUpCountersMu.RUnlock()
	if fn == nil {
		return PRFollowUpCounters{}, false
	}
	return fn()
}

// writePRFollowUpMetrics appends the PR follow-up series to b.
func writePRFollowUpMetrics(b *strings.Builder, hiveID string, writeHeader func(name, help, typ string)) {
	c, ok := prFollowUpCounters()
	if !ok {
		return
	}
	writeHeader("hive_pr_followup_resumed_total",
		"PR follow-up events delivered into the live session that authored the PR.", "counter")
	fmt.Fprintf(b, "hive_pr_followup_resumed_total{hive_id=%q} %d\n", hiveID, c.Resumed)

	writeHeader("hive_pr_followup_fallback_total",
		"PR follow-up events handed to the fresh-dispatch path, by reason.", "counter")
	writeReasonSeries(b, "hive_pr_followup_fallback_total", hiveID, c.Fallback)

	writeHeader("hive_pr_followup_skipped_total",
		"PRs with a follow-up pointer entering a skipped state (draft, fork, escalated), by reason.", "counter")
	writeReasonSeries(b, "hive_pr_followup_skipped_total", hiveID, c.Skipped)

	writeHeader("hive_pr_followup_deferred_total",
		"PR follow-up routing attempts deferred because the authoring session was busy.", "counter")
	fmt.Fprintf(b, "hive_pr_followup_deferred_total{hive_id=%q} %d\n", hiveID, c.Deferred)

	writeHeader("hive_pr_followup_handoffs_total",
		"Human-feedback PR follow-ups queued for a fresh kick, and those a kick has since delivered.", "counter")
	fmt.Fprintf(b, "hive_pr_followup_handoffs_total{hive_id=%q,state=%q} %d\n", hiveID, "queued", c.HandoffsQueued)
	fmt.Fprintf(b, "hive_pr_followup_handoffs_total{hive_id=%q,state=%q} %d\n", hiveID, "delivered", c.HandoffsDelivered)

	writeHeader("hive_pr_followup_pointers_pruned_total",
		"PR follow-up pointers deleted by the sweep, by reason.", "counter")
	writeReasonSeries(b, "hive_pr_followup_pointers_pruned_total", hiveID, c.Pruned)
}

func writeReasonSeries(b *strings.Builder, name, hiveID string, m map[string]int) {
	reasons := make([]string, 0, len(m))
	for r := range m {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(b, "%s{hive_id=%q,reason=%q} %d\n", name, hiveID, r, m[r])
	}
}

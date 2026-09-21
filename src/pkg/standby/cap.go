package standby

import "time"

// DispatchWindow is the trailing (rolling) window over which
// standby.daily_cap_per_contributor is counted. It mirrors rateLimitDayWindow
// in pkg/dashboard (`contribute_select.go`) and for the same reason: a
// calendar-day bucket resets at a fixed instant, which lets a burst straddle
// the boundary and spend two days' worth of donated slots in an hour. A
// sliding window anchored on "now" cannot be straddled.
const DispatchWindow = 24 * time.Hour

// DispatchesInWindow counts the dispatches that still count against the cap at
// now: those made less than DispatchWindow ago. A dispatch made exactly
// DispatchWindow ago has expired and does not count, which is what makes the
// cap "reset" without any reset event ever firing.
//
// A timestamp in the future — clock skew between the hub and whatever wrote
// the ledger — counts as in-window. Counting it fails closed; skipping it
// would let a skewed clock hand out free slots.
func DispatchesInWindow(dispatches []time.Time, now time.Time) int {
	n := 0
	for _, d := range dispatches {
		if d.IsZero() {
			continue
		}
		if now.Sub(d) < DispatchWindow {
			n++
		}
	}
	return n
}

// CapRemaining reports how many donated tasks this candidate may still be
// dispatched on this lane.
//
// A DailyCap of zero or less means nothing is dispatched, and that is the
// default. It is what makes the standby configuration safe to adopt before the
// dispatch path exists: a hive can switch standby on, watch the counts, and
// dispatch nothing.
func CapRemaining(c Candidate, p LanePolicy, now time.Time) int {
	if p.DailyCap <= 0 {
		return 0
	}
	remaining := p.DailyCap - DispatchesInWindow(c.Dispatches, now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// RecordDispatch returns the dispatch list with a dispatch at `at` added and
// everything that has fallen out of the window dropped.
//
// The cap decrements at dispatch, not at completion: an abandoned donated task
// still costs a slot. Call this when the task is assigned.
//
// It does not mutate its argument — the caller's slice is left as it was, so a
// caller holding ledger state cannot be surprised by aliasing.
func RecordDispatch(dispatches []time.Time, at time.Time) []time.Time {
	out := make([]time.Time, 0, len(dispatches)+1)
	for _, d := range dispatches {
		if d.IsZero() {
			continue
		}
		if at.Sub(d) < DispatchWindow {
			out = append(out, d)
		}
	}
	return append(out, at)
}

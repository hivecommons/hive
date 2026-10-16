package main

import "time"

const (
	// continuousWakeMinGap bounds how often the continuous wake may run an
	// early eval cycle, independent of how many continuous agents come due.
	continuousWakeMinGap = 30 * time.Second
	// continuousWakePoll is how often the wake re-checks the governor when no
	// continuous re-kick is scheduled yet (a kick outcome lands asynchronously
	// from the agent supervisor, outside any eval tick).
	continuousWakePoll = 30 * time.Second
	// continuousWakeMinDelay keeps a timer that is "due now" from spinning.
	continuousWakeMinDelay = time.Second
)

// continuousWakeDecision is the pure core of the continuous re-kick wake.
// next is the earliest scheduled continuous NextKick (zero = none), lastWake
// is when the wake last ran an early eval. It reports whether an early eval
// cycle is due right now and, if not, how long to sleep before re-checking.
func continuousWakeDecision(next, now, lastWake time.Time) (delay time.Duration, due bool) {
	if next.IsZero() {
		return continuousWakePoll, false
	}
	if now.Before(next) {
		d := next.Sub(now)
		if d > continuousWakePoll {
			d = continuousWakePoll
		}
		if d < continuousWakeMinDelay {
			d = continuousWakeMinDelay
		}
		return d, false
	}
	if !lastWake.IsZero() {
		if gap := now.Sub(lastWake); gap < continuousWakeMinGap {
			return continuousWakeMinGap - gap, false
		}
	}
	return 0, true
}

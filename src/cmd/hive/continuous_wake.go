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
	// continuousWakeStallBackoff is the first hold applied when a wake-driven
	// eval cycle leaves the due re-kick exactly where it was; each further
	// stall on the same re-kick doubles it, up to continuousWakeStallMax.
	continuousWakeStallBackoff = time.Minute
	continuousWakeStallMax     = 15 * time.Minute
)

// continuousWakeHold backs the wake off a re-kick that the eval cycle keeps
// skipping. Governor.NextContinuousKickAt reports any elapsed NextKick that is
// not Blocked, but runEvalCycle drops due agents after that decision for
// reasons the governor never sees — dashboard pause, first-scan deferral, the
// provider-budget gate, the self-upgrade drain. Without the hold the wake
// found the same past NextKick every continuousWakeMinGap and ran a full eval
// cycle each time, turning a 900s cadence into a 30s one for as long as the
// agent stayed gated. The hold is keyed on the (NextKick, agent) pair it saw
// stall, so any change to the schedule releases it at once.
type continuousWakeHold struct {
	next    time.Time
	who     string
	backoff time.Duration
	until   time.Time
}

// observe compares the earliest re-kick before and after a wake-driven eval
// cycle. An unchanged pair means the due agent was skipped: the hold starts
// (or doubles) and reports the back-off applied. Any change clears it.
func (h *continuousWakeHold) observe(before, after time.Time, who, afterWho string, now time.Time) (time.Duration, bool) {
	if before.IsZero() || !after.Equal(before) || afterWho != who {
		*h = continuousWakeHold{}
		return 0, false
	}
	if h.backoff > 0 && h.next.Equal(before) && h.who == who {
		h.backoff *= 2
		if h.backoff > continuousWakeStallMax {
			h.backoff = continuousWakeStallMax
		}
	} else {
		h.backoff = continuousWakeStallBackoff
	}
	h.next, h.who, h.until = before, who, now.Add(h.backoff)
	return h.backoff, true
}

// untilFor returns when the hold on this exact re-kick lifts, or zero when
// the hold does not apply to it.
func (h continuousWakeHold) untilFor(next time.Time, who string) time.Time {
	if h.until.IsZero() || !h.next.Equal(next) || h.who != who {
		return time.Time{}
	}
	return h.until
}

// continuousWakeDecision is the pure core of the continuous re-kick wake.
// next is the earliest scheduled continuous NextKick (zero = none), lastWake
// is when the wake last ran an early eval, holdUntil is the stall hold on
// that re-kick (zero = none). It reports whether an early eval cycle is due
// right now and, if not, how long to sleep before re-checking.
func continuousWakeDecision(next, now, lastWake, holdUntil time.Time) (delay time.Duration, due bool) {
	if next.IsZero() {
		return continuousWakePoll, false
	}
	if now.Before(next) {
		return clampWakeDelay(next.Sub(now)), false
	}
	if !holdUntil.IsZero() && now.Before(holdUntil) {
		// Held: re-check at the poll cadence so a schedule change (the agent
		// got kicked by the cadence tick, or was unpaused) releases it early.
		return clampWakeDelay(holdUntil.Sub(now)), false
	}
	if !lastWake.IsZero() {
		if gap := now.Sub(lastWake); gap < continuousWakeMinGap {
			return continuousWakeMinGap - gap, false
		}
	}
	return 0, true
}

// clampWakeDelay bounds a wake sleep to [continuousWakeMinDelay, continuousWakePoll].
func clampWakeDelay(d time.Duration) time.Duration {
	if d > continuousWakePoll {
		d = continuousWakePoll
	}
	if d < continuousWakeMinDelay {
		d = continuousWakeMinDelay
	}
	return d
}

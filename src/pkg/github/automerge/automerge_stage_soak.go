package automerge

// Soak bookkeeping for the stage store. The contributor lane's soak clock
// (ContributorSoakTracker) is in-memory and per head; the stage record is
// what the dashboard reads. Each pass writes the soak it is holding the
// PR's head for — the lane's own soak after admission (095,
// `contributor-lane:soaking(remaining=…)`) or the soak a trust-blocked head
// sits through at 060 (RouteHuman's "soak Nm remaining") — so the Activity
// Monitor can count one clock down in both places and tell a head that
// moved on from one still soaking.

import (
	"regexp"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// soakRemainingRE pulls the time left out of the lane's
// `contributor-lane:soaking(remaining=12m0s)` reason.
var soakRemainingRE = regexp.MustCompile(`remaining=([0-9hms.]+)`)

// soakRemainingFromReason parses the soak time left from a soaking reason.
func soakRemainingFromReason(reason string) (time.Duration, bool) {
	m := soakRemainingRE.FindStringSubmatch(reason)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// soakWindowFor derives the soak the pass is holding head for from the
// lane's reason and the human-route evidence: the head, when it was first
// green and the window. ok is false when the head is not soaking.
func soakWindowFor(reason, head string, route *stagelabels.Evidence, window time.Duration, now time.Time) (startedAt time.Time, win time.Duration, ok bool) {
	if head == "" {
		return time.Time{}, 0, false
	}
	if left, parsed := soakRemainingFromReason(reason); parsed && window > 0 {
		if left > window {
			left = window
		}
		return now.Add(left - window), window, true
	}
	// RouteHuman measures the soak only on a green head (SoakElapsed is 0
	// both when the head is not green and when it was first seen green
	// this pass), so the checks decide; a head past the window still
	// carries the clock so the dashboard can say it is soaked.
	if route != nil && route.Soak > 0 && route.SoakElapsed >= 0 && routeHeadGreen(route) {
		return now.Add(-route.SoakElapsed), route.Soak, true
	}
	return time.Time{}, 0, false
}

// routeHeadGreen mirrors the lane's green test in routeHuman: every gating
// check on the head known, reported and passing.
func routeHeadGreen(route *stagelabels.Evidence) bool {
	c := route.Checks
	return c.Known && c.Total > 0 && c.Pending == 0 && len(c.Red) == 0
}

// noteSoak writes (or clears) the record's soak for this pass.
func noteSoak(store *stagelabels.Store, key, reason, head string, route *stagelabels.Evidence, window time.Duration, now time.Time) {
	if store == nil {
		return
	}
	startedAt, win, ok := soakWindowFor(reason, head, route, window, now)
	if !ok {
		store.ClearSoak(key)
		return
	}
	store.NoteSoak(key, head, startedAt, win)
}

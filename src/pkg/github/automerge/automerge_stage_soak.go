package automerge

// Soak bookkeeping for the stage store. The contributor lane's soak clock
// (ContributorSoakTracker) is in-memory and per head; the stage record is
// what the dashboard reads. Each pass writes the soak it is holding the
// PR's head for — the lane's own soak after admission (095,
// `contributor-lane:soaking`, with the clock read from the tracker) or the
// soak a trust-blocked head sits through at 060 (RouteHuman's "soak Nm
// remaining") — so the Activity Monitor can count one clock down in both
// places and tell a head that moved on from one still soaking. The clock
// is a fact about the head (when it went green, the window), so an
// unchanged pass writes nothing: the record stays byte-identical and the
// dashboard does not read every sweep as a fresh hive note
// (hivecommons/hive#15729).

import (
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// soakRemainingRE pulls the time left out of the
// `contributor-lane:soaking(remaining=12m0s)` reason the lane wrote before
// the clock moved onto the record. Kept so a stored reason of that shape
// still reads as a clock; nothing writes it any more.
var soakRemainingRE = regexp.MustCompile(`remaining=([0-9hms.]+)`)

// soakRemainingFromReason parses the soak time left from a soaking reason
// of the pre-record shape; false for the bare `contributor-lane:soaking`.
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
// lane's reason, the tracker's first-green time for the head (firstGreen;
// zero when the tracker has not seen it) and the human-route evidence:
// the head, when it was first green and the window. ok is false when the
// head is not soaking.
func soakWindowFor(reason, head string, route *stagelabels.Evidence, window time.Duration, firstGreen, now time.Time) (startedAt time.Time, win time.Duration, ok bool) {
	if head == "" {
		return time.Time{}, 0, false
	}
	if strings.HasPrefix(reason, contributorLaneReasonSoaking) && window > 0 {
		if left, parsed := soakRemainingFromReason(reason); parsed {
			if left > window {
				left = window
			}
			return now.Add(left - window), window, true
		}
		if !firstGreen.IsZero() && !firstGreen.After(now) {
			return firstGreen, window, true
		}
		// The lane says soaking but this process has no clock for the
		// head (the tracker is in memory): the soak reads as starting
		// now, and noteSoak keeps a clock already on the record instead.
		return now, window, true
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

// noteSoak writes (or clears) the record's soak for this pass. A clock
// the record already carries for the same head is kept when the tracker
// has none to offer (firstGreen zero): the record is the memory that
// survives a restart, and re-stamping it from "now" would restart the
// countdown the dashboard shows.
func noteSoak(store *stagelabels.Store, key, reason, head string, route *stagelabels.Evidence, window time.Duration, firstGreen, now time.Time) {
	if store == nil {
		return
	}
	startedAt, win, ok := soakWindowFor(reason, head, route, window, firstGreen, now)
	if !ok {
		store.ClearSoak(key)
		return
	}
	if firstGreen.IsZero() && strings.HasPrefix(reason, contributorLaneReasonSoaking) {
		if rec, has := store.Get(key); has {
			if soakHead, at, recWin, recorded := rec.Soak(); recorded && soakHead == head && recWin == win && !at.After(now) {
				startedAt = at
			}
		}
	}
	store.NoteSoak(key, head, startedAt, win)
}

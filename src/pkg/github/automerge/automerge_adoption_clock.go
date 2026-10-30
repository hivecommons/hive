package automerge

import (
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// One predicate for "hive adopts this PR" (hivecommons/hive#15565,
// #15566, #15609, #15312: PRs by an offline contributor clanker sat at
// 072 — conflicting, changes requested, an optional check red, a fork the
// App cannot update — while the tracker offered Adopt now and the operator
// had to click it). The sweep and the tracker used to keep separate
// clocks: the engine measured the offline wait from the author's freshest
// heartbeat (which a reconnect or a task completion moves, so the window
// restarted every time the clanker flapped), the tracker measured the
// nudge grace from the first nudge, and the presence pill projected a
// third variant. They are one function now, AutoAdoption: when it says the
// adoption is due the sweep performs it on that cycle, attributed to the
// hive with the trigger; the tracker's "adopt?" / grace-elapsed offer and
// its adopt-at projection are the same verdict. The operator's Adopt now
// stays as the manual override for a row the predicate has not reached.

// AdoptionClock is the timing slice of StageLabelPolicy the predicate
// needs; the dashboard builds one from governor.labels.stage so the two
// sides cannot drift.
type AdoptionClock struct {
	// GraceOnline / GraceOffline are the nudge graces when no live presence
	// source exists (and GraceOnline is the online wait with one).
	GraceOnline  time.Duration
	GraceOffline time.Duration
	// AdoptAfterOffline / AdoptAfterUnregistered are the waits with live
	// presence for a registered-but-offline relay and for an author with no
	// profile on this hive.
	AdoptAfterOffline      time.Duration
	AdoptAfterUnregistered time.Duration
	// AdoptOfflineGrace is the retry wait after an adoption attempt that
	// opened nothing (hivecommons/hive#15307).
	AdoptOfflineGrace time.Duration
}

// Defaults applied where the clock is zero (mirroring pkg/config).
const (
	defaultGraceOnline            = 2 * time.Hour
	defaultGraceOffline           = 15 * time.Minute
	defaultAdoptAfterOffline      = 15 * time.Minute
	defaultAdoptAfterUnregistered = 15 * time.Minute
	defaultAdoptOfflineGrace      = 30 * time.Minute
)

func (c AdoptionClock) graceOnline() time.Duration {
	return durationOr(c.GraceOnline, defaultGraceOnline)
}

func (c AdoptionClock) graceOffline() time.Duration {
	return durationOr(c.GraceOffline, defaultGraceOffline)
}

func (c AdoptionClock) afterOffline() time.Duration {
	return durationOr(c.AdoptAfterOffline, defaultAdoptAfterOffline)
}

func (c AdoptionClock) afterUnregistered() time.Duration {
	return durationOr(c.AdoptAfterUnregistered, defaultAdoptAfterUnregistered)
}

func (c AdoptionClock) offlineGrace() time.Duration {
	return durationOr(c.AdoptOfflineGrace, defaultAdoptOfflineGrace)
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// AdoptionClock is the policy's timing slice.
func (p StageLabelPolicy) AdoptionClock() AdoptionClock {
	return AdoptionClock{
		GraceOnline:            p.GraceOnline,
		GraceOffline:           p.GraceOffline,
		AdoptAfterOffline:      p.AdoptAfterOffline,
		AdoptAfterUnregistered: p.AdoptAfterUnregistered,
		AdoptOfflineGrace:      p.AdoptOfflineGrace,
	}
}

// AdoptionVerdict is what AutoAdoption decided for a head.
type AdoptionVerdict struct {
	// Running is true when the adoption clock is running on this head: a
	// nudged clanker PR whose head has not moved since the nudge and that
	// is not already adopted (or whose recorded attempt opened nothing and
	// is retried). At is zero when it is not.
	Running bool
	// Due is true when hive adopts on this sweep.
	Due bool
	// Reason is the AdoptReason* that fires (or will fire at At).
	Reason string
	// At is when the verdict turns due.
	At time.Time
}

// AutoAdoption is the one predicate behind automatic adoption, the
// tracker's Adopt offer and its adopt-at projection. The clock runs once
// the author was nudged at head (a new head restarts it: a clanker that is
// pushing keeps its PR). Then:
//
//   - a registered relay that is online keeps the online grace from the
//     first nudge (its hive may be mid-fix);
//   - a registered relay that is offline is adopted after AdoptAfterOffline.
//     A heartbeat after the nudge — a reconnect, a queued task completing,
//     an acknowledgement — extends the wait by at most one window: the
//     clock counts from min(last heartbeat, nudge+wait), never from every
//     fresh heartbeat (that is how #15566 slid for an hour);
//   - an author with no profile here is adopted after AdoptAfterUnregistered;
//   - without a presence source the nudge-time grace applies.
//
// An adoption already recorded for the head that opened nothing is retried
// under AdoptOfflineGrace from the attempt, with the same one-window
// heartbeat cap, while the clanker stays offline or unregistered.
func AutoAdoption(rec stagelabels.Record, head string, clock AdoptionClock, now time.Time, presence ContributorPresence, hasPresence bool) AdoptionVerdict {
	start := rec.NudgeStart()
	if head == "" || rec.NudgeHead != head || rec.NudgeCount == 0 || start.IsZero() {
		return AdoptionVerdict{}
	}
	if rec.Adopted(head) || rec.AdoptedInPlace() {
		return adoptionRetryVerdict(rec, head, clock, now, presence, hasPresence)
	}
	var v AdoptionVerdict
	switch {
	case !hasPresence:
		grace := clock.graceOffline()
		if rec.NudgeOnline {
			grace = clock.graceOnline()
		}
		v = AdoptionVerdict{Running: true, Reason: AdoptReasonGrace, At: start.Add(grace)}
	case presence.Registered && presence.Online:
		v = AdoptionVerdict{Running: true, Reason: AdoptReasonGrace, At: start.Add(clock.graceOnline())}
	case presence.Registered:
		wait := clock.afterOffline()
		v = AdoptionVerdict{Running: true, Reason: AdoptReasonOffline, At: cappedSince(start, presence.LastSeen, wait).Add(wait)}
	default:
		v = AdoptionVerdict{Running: true, Reason: AdoptReasonUnregistered, At: start.Add(clock.afterUnregistered())}
	}
	v.Due = !now.Before(v.At)
	return v
}

// cappedSince is where an offline wait counts from: the later of start
// and the last heartbeat, but a heartbeat buys at most one window past
// start.
func cappedSince(start, lastSeen time.Time, wait time.Duration) time.Time {
	if !lastSeen.After(start) {
		return start
	}
	if cap := start.Add(wait); lastSeen.After(cap) {
		return cap
	}
	return lastSeen
}

// adoptionRetryVerdict is AutoAdoption for a head whose recorded adoption
// attempt opened nothing (hivecommons/hive#15307): the supersede's replay
// conflicted and the attempt was recorded, so the plain clock never fired
// again while the PR sat at 072/085. With the clanker offline or
// unregistered the head is admitted again after AdoptOfflineGrace; an
// online clanker keeps the PR. A live adoption (a copy open, the branch
// held in place) is never re-admitted here.
func adoptionRetryVerdict(rec stagelabels.Record, head string, clock AdoptionClock, now time.Time, presence ContributorPresence, hasPresence bool) AdoptionVerdict {
	if !rec.AdoptionAttemptedNothingOpened(head) || rec.AdoptedAt.IsZero() {
		return AdoptionVerdict{}
	}
	if hasPresence && presence.Registered && presence.Online {
		return AdoptionVerdict{}
	}
	wait := clock.offlineGrace()
	since := rec.AdoptedAt
	if hasPresence {
		since = cappedSince(since, presence.LastSeen, wait)
	}
	v := AdoptionVerdict{Running: true, Reason: adoptionRetryReason(presence, hasPresence), At: since.Add(wait)}
	v.Due = !now.Before(v.At)
	return v
}

package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Why a queued fix leaves a contributor's fix queue. Each caller of
// fixqueue.Scheduler.RemoveFor passes its own cause, logged as `reason` on
// the "[contribute-ws] fix queue remove" line, so no removal is silent. A
// queued fix once vanished four seconds after it was queued, with nothing
// in the log to say which path took it.
const (
	// fixRemoveLeftStage072: the stage engine moved the PR out of 072 (Resolve).
	fixRemoveLeftStage072 = "left-stage-072"
	// fixRemovePRClosed: the dispatch check read the PR merged or closed.
	fixRemovePRClosed = "pr-closed"
	// fixRemoveRelayGaveUp: the relay failed the fix fixTaskMaxAttempts times.
	fixRemoveRelayGaveUp = "relay-gave-up"
	// fixRemoveRerouteCancelled: an operator's reroute lapsed or moved on.
	fixRemoveRerouteCancelled = "reroute-cancelled"
	// fixRemoveFleetCardPrefix + a fleetReworkGone* check: the fleet card
	// found the PR no longer open for the contributor.
	fixRemoveFleetCardPrefix = "fleet-card-not-open:"
)

// The check in reworkCandidateGone that found a rework candidate is no
// longer an open PR of the contributor's.
const (
	fleetReworkGoneAdopted     = "adopted-by-hive"
	fleetReworkGoneNotListed   = "not-in-open-listing"
	fleetReworkGoneForgeClosed = "forge-cache-closed"
	fleetReworkGoneSuperseded  = "superseded"
	fleetReworkGoneUnseen      = "stage-record-unseen"
)

// predates reports whether the open-PR listing ran before the evidence that
// a PR it does not carry is open: the fix was enqueued after the listing,
// or the PR's stage record sits in 071/072 (the hive waits on the author)
// and a stage pass observed it after the listing. Such a listing cannot
// vouch that the PR left. A listing whose time is unknown predates nothing.
func (idx openPRIndex) predates(rec stagelabels.Record, hasRec bool, enqueuedAt time.Time) bool {
	if idx.listedAt.IsZero() {
		return false
	}
	if enqueuedAt.After(idx.listedAt) {
		return true
	}
	return hasRec && rec.Stage.WaitsOnPush() && !rec.Superseded() && rec.LastSeen.After(idx.listedAt)
}

// statusListedAt is StatusPayload.ListedAt for a snapshot built from
// actionable: when that enumeration ran. Zero without one.
func statusListedAt(actionable *github.ActionableResult) time.Time {
	if actionable == nil {
		return time.Time{}
	}
	return actionable.GeneratedAt
}

// retryReturnedFixAfter dispatches user's queued fixes again once backoff
// has passed. A fix the relay handed back without starting (it was polling
// another hive) waits out fixTaskRetryBackoff in the queue; otherwise only
// the relay's next `ready` here or a fresh nudge (the same head is not
// re-nudged for auto_merge.nudge_repeat_after) would offer it again. The
// interrupt respects the scheduler's backoff, slots and focus window, so an
// extra call never hands out more than a Nudge would.
func (h *ContributeWSHub) retryReturnedFixAfter(user string, backoff time.Duration) {
	after := h.fixRetryAfter
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	after(backoff, func() {
		if h.stopCh != nil {
			select {
			case <-h.stopCh:
				return // the hub closed while the fix waited
			default:
			}
		}
		if conns, online := h.liveConnectionsFor(user, time.Now()); online {
			h.dispatchFixes(user, conns, false)
		}
	})
}

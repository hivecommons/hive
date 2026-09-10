package dashboard

// Contributor-side failure-streak detection (kubestellar/hive#6450).
//
// When a contributor relay's agent backend dies at startup (e.g. a container
// launched without its CLI credential, so every headless run exits within
// seconds), the relay keeps claiming assignments and failing them. Each
// failure books a per-ISSUE cooldown/quarantine (#2435) and degrades the
// contributor's standing — but before this file nothing on the contributor
// side surfaced that this was happening or why. The contributor experience
// was "my hive statistics get worse the more I use the tool".
//
// The heuristic here is the one #6450 proposes: N consecutive sub-minute
// failures from one identity is a dying runtime, not N distinct hard work
// items. Once the streak reaches the threshold, selectTask refuses the next
// claim for a short window with an explicit, machine-readable
// task_unavailable reason (contributor_failure_streak) and a human-readable
// message naming the streak and the pause expiry — telling the contributor at
// claim time prevents the next N failures instead of booking them.
//
// Deliberate boundaries:
//   - The signal is HUB-measured (assignment-to-task_failed wall clock), never
//     the client-declared failure_kind — routing on a self-reported value is
//     the ROUTE half of #2547 and stays undecided. A relay cannot fake its way
//     INTO or OUT of the streak with a declared kind.
//   - A failure with no measurable duration (the task was adopted on reconnect
//     without a fresh assignment) is NOT counted: better to miss a streak than
//     to invent one from a clock we never started.
//   - A failure slower than the fast-failure bound RESETS the streak: the
//     runtime demonstrably ran for a while, so the "dying at startup" theory is
//     void and the per-issue cooldowns are the right tool again.
//   - The pause is per-identity and expires on its own. After expiry the
//     contributor gets one probe assignment; if it also dies sub-minute the
//     streak (still above threshold) books the next pause immediately, so a
//     still-broken runtime costs one issue-cooldown per window instead of a
//     continuous stream.
//   - In-memory only. A hub restart forgets streaks; the per-issue failure
//     ledger (#2435) remains the durable protection.

import (
	"fmt"
	"time"
)

const (
	// contributorFailureStreakThreshold is how many CONSECUTIVE fast failures
	// one identity books before its claims are paused. Mirrors the per-issue
	// consecutiveFailureQuarantineThreshold so "the same contributor keeps
	// insta-failing" trips at the same rate as "the same issue keeps failing".
	contributorFailureStreakThreshold = 3
	// contributorFastFailureMax is the assignment-to-failure duration at or
	// under which a failure reads as "the runtime died at startup" rather than
	// "the work was attempted". #6450's reported case failed in seconds.
	contributorFastFailureMax = 60 * time.Second
	// contributorFailureStreakPause is how long claims are refused once the
	// threshold trips, measured from the streak's most recent failure. Matches
	// the "you are in a 10-minute failure cooldown" shape proposed in #6450.
	contributorFailureStreakPause = 10 * time.Minute
)

// contributorFailureStreak is one identity's run of consecutive fast failures.
type contributorFailureStreak struct {
	// Count of consecutive sub-contributorFastFailureMax failures.
	Count int
	// LastAt is when the most recent counted failure was booked; the pause
	// window is measured from here.
	LastAt time.Time
	// LastReason is the relay-reported reason of the most recent counted
	// failure, kept only to make the refusal message concrete.
	LastReason string
}

// recordContributorFastFailure books one task_failed against the identity's
// streak. duration is hub-measured assignment-to-failure wall clock; zero or
// negative means "unknown" (adopted task) and is ignored. A slow failure
// resets the streak — see the file comment.
func (h *ContributeWSHub) recordContributorFastFailure(identity string, duration time.Duration, reason string) {
	if identity == "" || duration <= 0 {
		return
	}
	h.failureStreakMu.Lock()
	defer h.failureStreakMu.Unlock()
	if h.contributorFailureStreaks == nil {
		h.contributorFailureStreaks = make(map[string]contributorFailureStreak)
	}
	if duration > contributorFastFailureMax {
		delete(h.contributorFailureStreaks, identity)
		return
	}
	rec := h.contributorFailureStreaks[identity]
	rec.Count++
	rec.LastAt = time.Now()
	rec.LastReason = reason
	h.contributorFailureStreaks[identity] = rec
}

// resetContributorFailureStreak clears the identity's streak. Called on a
// genuine task_complete: the runtime demonstrably works.
func (h *ContributeWSHub) resetContributorFailureStreak(identity string) {
	if identity == "" {
		return
	}
	h.failureStreakMu.Lock()
	delete(h.contributorFailureStreaks, identity)
	h.failureStreakMu.Unlock()
}

// contributorFailureStreakActive reports whether the identity's claims are
// currently paused, and if so returns the streak count and the pause expiry.
// Entries whose pause has lapsed are left in place (not pruned) so a
// still-broken runtime's next fast failure re-arms the pause immediately.
func (h *ContributeWSHub) contributorFailureStreakActive(identity string, now time.Time) (bool, contributorFailureStreak, time.Time) {
	if identity == "" {
		return false, contributorFailureStreak{}, time.Time{}
	}
	h.failureStreakMu.Lock()
	defer h.failureStreakMu.Unlock()
	rec, ok := h.contributorFailureStreaks[identity]
	if !ok || rec.Count < contributorFailureStreakThreshold {
		return false, rec, time.Time{}
	}
	until := rec.LastAt.Add(contributorFailureStreakPause)
	if !now.Before(until) {
		return false, rec, time.Time{}
	}
	return true, rec, until
}

// contributorFailureStreakMessage renders the human-readable half of the
// refusal, naming the streak, the fast-failure bound, and the pause expiry so
// a relay operator reading their own log learns why work stopped arriving —
// the exact visibility gap #6450 reports.
func contributorFailureStreakMessage(rec contributorFailureStreak, until time.Time) string {
	msg := fmt.Sprintf(
		"your last %d assignments each failed within %s of assignment — this usually means the agent runtime is dying at startup (missing credential, misconfigured backend); check the relay's agent logs. Assignments paused until %s.",
		rec.Count, contributorFastFailureMax, until.UTC().Format(time.RFC3339))
	if rec.LastReason != "" {
		msg += " Last reported failure: " + rec.LastReason
	}
	return msg
}

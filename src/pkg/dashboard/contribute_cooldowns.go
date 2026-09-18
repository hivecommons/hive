package dashboard

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

// failedTaskCooldownMinutes is the SHORT cooldown applied to an issue when a
// contributor reports task_failed (#2435). It is deliberately far shorter than
// the completion cooldowns above: a failure is often purely environmental
// (expired token, model outage, transient network) and the issue is likely
// still perfectly workable, so we must not park it for long. This short window
// is only large enough to break the tight reject/re-offer livelock — where a
// just-failed issue at the head of the deterministic scan order is handed
// straight back out ahead of the whole rest of the queue — while still letting a
// legitimate retry happen within a few minutes.
const failedTaskCooldownMinutes = 10

// consecutiveFailureQuarantineThreshold is how many consecutive failures an
// issue may accumulate (weighted — see permanentFailureWeight) before it is
// QUARANTINED for the longer quarantineCooldownHours window (#2435 remedy 2).
// A poison issue that nobody can complete burns at most this many assignments
// before it is parked for hours instead of minutes, so it stops starving the
// queue. The counter resets to zero on the issue's next completion.
const consecutiveFailureQuarantineThreshold = 3

// quarantineCooldownHours is the LONGER cooldown applied once an issue crosses
// consecutiveFailureQuarantineThreshold. It parks a reliably-failing issue for
// hours (long enough to stop it dominating the queue) without locking it as long
// as a genuine week-long completion cooldown — the issue may become workable
// again (e.g. a dependency merges) and should re-enter circulation the same day.
const quarantineCooldownHours = 6

// permanentFailureWeight is how much a permanent failure (msg.Permanent — the
// relay exhausted its per-task CLI-restart budget and will not retry, see
// bin/contributor-relay.js) counts toward consecutiveFailureQuarantineThreshold.
// A permanent failure is a strong "nobody here can do this" signal, so it
// advances the quarantine counter faster than an ordinary (possibly transient)
// failure. With a weight of 3 and a threshold of 3, a single permanent failure
// quarantines the issue immediately.
const permanentFailureWeight = 3

// cooldownEnabled reports whether post-completion cooldown gating is turned on
// for this hive. It reads the operator toggle (Config.Hub.ContributeCooldownEnabled)
// through the config resolver, which defaults to ENABLED when unset. A hub built
// without a Config (direct-in-test construction) is treated as enabled so the
// historical default behavior is preserved.
func (h *ContributeWSHub) cooldownEnabled() bool {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return true
	}
	return h.server.deps.Config.Hub.IsContributeCooldownEnabled()
}

// configuredWithPRCooldown returns the operator-configured WITH-PR completion
// cooldown duration. It reads Config.Hub.ContributeCooldownHoursOrDefault() —
// which yields the 168h default when unset — and falls back to the
// completedTaskCooldownHours const when no Config is present (tests). It does NOT
// consider whether cooldown is enabled; callers gate on cooldownEnabled().
func (h *ContributeWSHub) configuredWithPRCooldown() time.Duration {
	if h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		return completedTaskCooldownHours * time.Hour
	}
	return time.Duration(h.server.deps.Config.Hub.ContributeCooldownHoursOrDefault()) * time.Hour
}

// cooldownForLocked returns the cooldown duration to apply to key. Callers must
// already hold completedMu. When no per-task override was recorded (older
// on-disk entries, or hubs built directly in tests) it falls back to the
// operator-configured with-PR cooldown (default completedTaskCooldownHours) — the
// original, conservative default.
func (h *ContributeWSHub) cooldownForLocked(key string) time.Duration {
	if h.completedTaskCooldown != nil {
		if d, ok := h.completedTaskCooldown[key]; ok {
			return d
		}
	}
	return h.configuredWithPRCooldown()
}

// isTaskInCooldown is the GitHub-shaped wrapper retained for the many call
// sites that legitimately hold a repo and an issue number. Identity-keyed
// callers (ReadyQueue, selectTask) use isTaskInCooldownKey directly so external
// work gets its own cooldown instead of sharing "repo#0" (kubestellar/hive#4245).
func (h *ContributeWSHub) isTaskInCooldown(repo string, number int) bool {
	return h.isTaskInCooldownKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) isTaskInCooldownKey(key string) bool {
	// Operator kill-switch: when cooldown is disabled, no completed issue is ever
	// gated out of the queue for cooldown. Completion HISTORY is still recorded by
	// markTaskCompleted (stats/audit, #2356 duplicate detection) and failure
	// quarantine is unaffected — this only stops cooldown from EXCLUDING work.
	if !h.cooldownEnabled() {
		return false
	}
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.completedTasks[key]
	if !ok {
		return false
	}
	if time.Since(t) > h.cooldownForLocked(key) {
		delete(h.completedTasks, key)
		delete(h.completedTaskCooldown, key)
		delete(h.completedTaskPRURL, key)
		return false
	}
	return true
}

// cooldownExpiryKey returns when this issue's COMPLETION cooldown lapses, or
// the zero time when none is in force (#6902 evidence).
//
// Read-only by construction: unlike isTaskInCooldownKey it never prunes an
// expired entry, because it is called only to explain a refusal that gate has
// already made on this same pass — pruning here would mean the explanation
// mutated the state it is explaining. It reads the SAME map and the SAME
// per-key window, so the timestamp it reports is the one actually enforced.
func (h *ContributeWSHub) cooldownExpiryKey(key string) time.Time {
	if key == "" || !h.cooldownEnabled() {
		return time.Time{}
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.completedTasks[key]
	if !ok {
		return time.Time{}
	}
	return t.Add(h.cooldownForLocked(key))
}

// failureCooldownExpiryKey returns when this issue's FAILURE cooldown (or the
// longer quarantine window, whichever currently applies) lapses. Zero when no
// failure is on record. Read-only for the same reason as cooldownExpiryKey.
func (h *ContributeWSHub) failureCooldownExpiryKey(key string) time.Time {
	if key == "" {
		return time.Time{}
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.failedTasks[key]
	if !ok {
		return time.Time{}
	}
	return t.Add(h.failureCooldownForLocked(key))
}

// recordTaskFailure books a task_failed against an issue (#2435). It stamps the
// short failure cooldown and advances the issue's consecutive-failure counter
// (a permanent failure advances it by permanentFailureWeight rather than one),
// so a reliably-failing issue crosses consecutiveFailureQuarantineThreshold and
// earns the longer quarantine window instead of being handed straight back out.
// The counter is reset on completion (see markTaskCompleted).
func (h *ContributeWSHub) recordTaskFailure(repo string, number int, permanent bool) {
	h.recordTaskFailureKey(worksource.Ref{Repo: repo, Number: number}.Key(), permanent)
}

// recordTaskFailureForTask books a failure against the assignment's own
// canonical identity (kubestellar/hive#4245). Every caller already holds the
// WSTaskAssign, so routing through it keeps external work's failure history and
// quarantine separate instead of merging onto "repo#0".
func (h *ContributeWSHub) recordTaskFailureForTask(task *WSTaskAssign, permanent bool) {
	h.recordTaskFailureKey(task.identityKey(), permanent)
}

func (h *ContributeWSHub) recordTaskFailureKey(key string, permanent bool) {
	if key == "" {
		return
	}
	weight := 1
	if permanent {
		weight = permanentFailureWeight
	}
	h.completedMu.Lock()
	h.failedTasks[key] = time.Now()
	h.consecutiveFailures[key] += weight
	h.completedMu.Unlock()
	h.saveFailedTasks()
}

// bookReleaseCooldown stamps the SAME short cooldown recordTaskFailure stamps, but
// does NOT advance the issue's consecutive-failure counter (kubestellar/hive#4260).
//
// It exists for releases that are not failures OF THE ISSUE. The disconnect path
// books a cooldown for one reason (#2356): while a relay is reconnecting, its issue
// has dropped out of activeIssues — the only double-assign guard — and selectTask
// could hand the same issue to a second session, so both reach "open a PR" and file
// duplicates. That guarantee needs the timestamp and nothing else.
//
// Routing it through recordTaskFailure also incremented consecutiveFailures, so three
// dropped sockets on one issue — unremarkable on a flaky connection across a long
// session — pushed it past consecutiveFailureQuarantineThreshold and parked a
// perfectly workable issue for quarantineCooldownHours with nothing having actually
// failed. The counter also feeds recentFailureCount, which deprioritises the issue
// behind never-failed peers in selectTask, so a disconnect quietly demoted work that
// was progressing fine.
//
// The #2356 window is byte-for-byte unchanged: failedTasks carries the same timestamp,
// isTaskInFailureCooldown reads it the same way, and with the count left at zero
// failureCooldownForLocked returns the same failedTaskCooldownMinutes it always did.
// Paths where the release IS evidence about the issue — task_failed, the relay's own
// watchdog giving up via "ready", and the wedged-task lease backstop — deliberately
// keep calling recordTaskFailure and keep counting.
func (h *ContributeWSHub) bookReleaseCooldown(repo string, number int) {
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	h.failedTasks[key] = time.Now()
	h.completedMu.Unlock()
	h.saveFailedTasks()
}

// clearReleaseCooldown withdraws a cooldown booked by bookReleaseCooldown once the
// release it was hedging against turns out not to have happened
// (kubestellar/hive#5322).
//
// The disconnect path books that cooldown speculatively: at the moment a socket
// drops the hub cannot know whether the relay is gone for good or reconnecting, so
// it stamps the #2356 window to stop a second session being handed the same issue
// during the gap. A lease-bound resume answers the question — the ORIGINAL relay is
// back and still on the ORIGINAL task, so no release ever occurred and the hedge has
// served its purpose. Leaving it stamped is what left a demonstrably in-flight issue
// carrying a release cooldown for the rest of the window: the ledger said "recently
// let go" about work nobody let go of, and the operator surfaces that read the
// failure ledger agreed.
//
// It is deliberately NARROW. It clears only the timestamp, and only when the issue
// carries NO consecutive-failure count — i.e. only a hedge booked by
// bookReleaseCooldown, never a cooldown earned through recordTaskFailure by a real
// task_failed, a watchdog give-up, or the wedged-task backstop. A resume therefore
// cannot launder a genuine failure record, and the #2356 duplicate-PR guarantee is
// intact because the very thing that clears the window is the original owner
// re-entering activeIssues, which is the stronger guard the window was standing in
// for.
func (h *ContributeWSHub) clearReleaseCooldown(repo string, number int) {
	key := fmt.Sprintf("%s#%d", repo, number)
	h.completedMu.Lock()
	_, booked := h.failedTasks[key]
	if booked && h.consecutiveFailures[key] == 0 {
		delete(h.failedTasks, key)
	} else {
		booked = false
	}
	h.completedMu.Unlock()
	if booked {
		h.saveFailedTasks()
	}
}

// failureCooldownForLocked returns how long, from the last failure time, an
// issue should be excluded from selection. It is the SHORT
// failedTaskCooldownMinutes normally, or the LONGER quarantineCooldownHours once
// the issue's consecutive-failure count has reached
// consecutiveFailureQuarantineThreshold. Callers must hold completedMu.
func (h *ContributeWSHub) failureCooldownForLocked(key string) time.Duration {
	if h.consecutiveFailures[key] >= consecutiveFailureQuarantineThreshold {
		return quarantineCooldownHours * time.Hour
	}
	return failedTaskCooldownMinutes * time.Minute
}

// isTaskInFailureCooldown reports whether an issue is currently excluded because
// of a recent failure — either the short post-failure cooldown or the longer
// quarantine (#2435). It self-heals in two stages so the failure-aware selection
// backstop (recentFailureCount) still has something to work with just after the
// short window lapses:
//   - Once the APPLICABLE window (short cooldown, or quarantine) has elapsed the
//     issue is admissible again → returns false.
//   - The consecutive-failure COUNT is only cleared once the full quarantine
//     window has elapsed. So an issue whose short cooldown just expired is
//     admissible but still carries its failure history, letting selectTask
//     deprioritise it behind never-failed peers rather than instantly restoring
//     it to the head of the queue. The count also resets on completion.
func (h *ContributeWSHub) isTaskInFailureCooldown(repo string, number int) bool {
	return h.isTaskInFailureCooldownKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) isTaskInFailureCooldownKey(key string) bool {
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	t, ok := h.failedTasks[key]
	if !ok {
		return false
	}
	// Keep the failure ledger for the full quarantine window (the longest window
	// we could apply), then clear timestamp AND count together so stale history
	// never lingers indefinitely. Retaining the timestamp past the short cooldown
	// is what preserves the consecutive-failure count for the selection backstop.
	if time.Since(t) > quarantineCooldownHours*time.Hour {
		delete(h.failedTasks, key)
		delete(h.consecutiveFailures, key)
		return false
	}
	// Inside the quarantine window: excluded only while within the currently
	// applicable cooldown (short cooldown, or the full quarantine once the count
	// crosses the threshold). Past that, the issue is admissible again but its
	// count remains on record — recentFailureCount reads it to deprioritise the
	// issue behind never-failed peers.
	return time.Since(t) <= h.failureCooldownForLocked(key)
}

// recentFailureCount returns the number of failures currently on record for an
// issue (0 when none/expired). selectTask uses it as a stable tie-break so that,
// among equally-admissible candidates, those without recent failures are offered
// before those that have failed recently — guaranteeing forward progress even if
// the failure cooldown has just elapsed (#2435 remedy 3 backstop).
func (h *ContributeWSHub) recentFailureCount(repo string, number int) int {
	return h.recentFailureCountKey(worksource.Ref{Repo: repo, Number: number}.Key())
}

func (h *ContributeWSHub) recentFailureCountKey(key string) int {
	if key == "" {
		return 0
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	return h.consecutiveFailures[key]
}

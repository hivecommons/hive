package main

import (
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// selfUpgradeDrainStaleAfter bounds how long a drain stays latched without a
// fresh upgrade instruction. The hub re-sends UpgradeTo on every heartbeat
// (heartbeatSendInterval) while the spoke is behind, so a drain that has not
// been re-checked for several beats means the hub stopped asking — the
// upgrade was cancelled or superseded — and kicks must not stay withheld.
const selfUpgradeDrainStaleAfter = 3 * heartbeatSendInterval

// selfUpgradeDrainNow is the drain clock, a seam for tests.
var selfUpgradeDrainNow = time.Now

// selfUpgradeDrain is the process-wide drain latch shared by the hub
// UpgradeCallback (which opens it) and the governor eval loop (which withholds
// new kicks while it is open).
var selfUpgradeDrain = &upgradeDrain{}

// upgradeDrain tracks a hub-instructed self-upgrade that is waiting for the
// agents' in-flight kicks to finish before the process exits (#11876).
type upgradeDrain struct {
	mu        sync.Mutex
	target    string
	startedAt time.Time
	checkedAt time.Time
	// deadline is startedAt plus the drain timeout of the open drain window.
	deadline time.Time
	// released is set once decide lets the upgrade proceed. Every path after
	// that point exits the process or blocks until SIGTERM, so kicks stay
	// withheld for good: a kick started now would be cut by the restart.
	released bool
}

// upgradeDrainDecision is the verdict for one upgrade instruction.
type upgradeDrainDecision struct {
	// Proceed is true when the upgrade may exit now.
	Proceed bool
	// TimedOut is true when Proceed was forced by the drain timeout while
	// kicks were still in flight; InFlight then names the kicks being cut.
	TimedOut bool
	// InFlight names the agents whose kicked turn has not ended yet.
	InFlight []string
	// Waited is how long this target has been draining.
	Waited time.Duration
	// Remaining is how long the drain will still wait at most.
	Remaining time.Duration
}

// decide reports whether a self-upgrade to target may proceed now. A drain is
// opened the first time kicks are found in flight and is keyed on target, so a
// newer build arriving mid-drain keeps the original deadline instead of
// restarting the window. timeout <= 0 disables draining.
func (d *upgradeDrain) decide(target string, inFlight []string, timeout time.Duration, now time.Time) upgradeDrainDecision {
	d.mu.Lock()
	defer d.mu.Unlock()

	if timeout <= 0 {
		d.target, d.startedAt, d.checkedAt, d.deadline = "", time.Time{}, time.Time{}, time.Time{}
		return upgradeDrainDecision{Proceed: true}
	}
	if len(inFlight) == 0 {
		dec := upgradeDrainDecision{Proceed: true}
		if !d.startedAt.IsZero() {
			dec.Waited = now.Sub(d.startedAt)
		}
		d.releaseLocked(target, now)
		return dec
	}

	if d.startedAt.IsZero() || now.Sub(d.checkedAt) >= selfUpgradeDrainStaleAfter {
		d.startedAt = now
	}
	d.target = target
	d.checkedAt = now
	d.deadline = d.startedAt.Add(timeout)

	waited := now.Sub(d.startedAt)
	if waited >= timeout {
		d.releaseLocked(target, now)
		return upgradeDrainDecision{Proceed: true, TimedOut: true, InFlight: inFlight, Waited: waited}
	}
	return upgradeDrainDecision{InFlight: inFlight, Waited: waited, Remaining: timeout - waited}
}

// Active reports whether new kicks should be withheld because a self-upgrade
// is draining. A drain the hub has stopped re-confirming expires on its own.
func (d *upgradeDrain) Active(now time.Time) (target string, active bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released {
		return d.target, true
	}
	if d.target == "" {
		return "", false
	}
	if now.Sub(d.checkedAt) >= selfUpgradeDrainStaleAfter {
		d.target, d.startedAt, d.checkedAt, d.deadline = "", time.Time{}, time.Time{}, time.Time{}
		return "", false
	}
	return d.target, true
}

// releaseLocked closes the drain window and latches the kick hold until the
// process exits.
func (d *upgradeDrain) releaseLocked(target string, now time.Time) {
	d.target = target
	d.startedAt = time.Time{}
	d.checkedAt = now
	d.deadline = time.Time{}
	d.released = true
}

// Deadline returns the latest moment the open drain will keep deferring the
// self-upgrade, for the heartbeat. ok is false when no drain is waiting
// (none opened, already released, or no longer re-confirmed by the hub).
func (d *upgradeDrain) Deadline(now time.Time) (deadline time.Time, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released || d.target == "" || d.deadline.IsZero() {
		return time.Time{}, false
	}
	if now.Sub(d.checkedAt) >= selfUpgradeDrainStaleAfter || !now.Before(d.deadline) {
		return time.Time{}, false
	}
	return d.deadline, true
}

// agentKicksInFlight names the running, unpaused agents whose most recent kick was
// delivered by THIS process and whose turn has not been classified as ended
// (agent.KickOutcome is settled by the pane poller / stall watchdog when the
// CLI returns to its prompt). Kicks seeded from persisted state predate
// processStart and cannot still be running here, so they are ignored.
func agentKicksInFlight(statuses map[string]*agent.AgentProcess, processStart time.Time) []string {
	var names []string
	for name, proc := range statuses {
		if proc == nil || proc.State != agent.StateRunning || proc.Paused || proc.LastKick == nil {
			continue
		}
		if !proc.LastKick.After(processStart) {
			continue
		}
		if proc.KickOutcome.Settled(proc.LastKick) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// upgradeDrainUntilForHeartbeat renders the drain deadline for the heartbeat,
// or "" when no self-upgrade is draining.
func upgradeDrainUntilForHeartbeat(now time.Time) string {
	if deadline, ok := selfUpgradeDrain.Deadline(now); ok {
		return deadline.UTC().Format(time.RFC3339)
	}
	return ""
}

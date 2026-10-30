package main

import (
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// selfUpgradeDrainStaleAfter bounds how long a drain stays latched without a
// fresh upgrade instruction. The hub re-sends UpgradeTo on every heartbeat
// (heartbeatSendInterval) while the spoke is behind, so a drain that has not
// been re-checked for several beats means the hub stopped asking — the
// upgrade was cancelled or superseded — and kicks must not stay withheld.
const selfUpgradeDrainStaleAfter = 3 * heartbeatSendInterval

// restartAnnounceSettle is how long the SIGTERM hook lets the SSE handler
// goroutines flush the "upgrading" frame before the next hook runs
// (#15788). One write on already-open sockets; well inside the grace period.
const restartAnnounceSettle = 100 * time.Millisecond

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
	// inFlight and timeout are the last decide inputs, kept so Report can
	// tell the hub WHAT the drain is waiting on and for how much longer.
	inFlight []string
	timeout  time.Duration
	// kickAt is when each in-flight agent's current turn was kicked, so
	// Report can say "reviewer 38m" (#14856). Names absent from the map are
	// reported with an unknown elapsed time.
	kickAt map[string]time.Time
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
		d.target, d.startedAt, d.checkedAt = "", time.Time{}, time.Time{}
		d.inFlight, d.timeout = nil, 0
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
	d.inFlight = append([]string(nil), inFlight...)
	d.timeout = timeout

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
		d.target, d.startedAt, d.checkedAt = "", time.Time{}, time.Time{}
		d.inFlight, d.timeout = nil, 0
		return "", false
	}
	return d.target, true
}

// Report describes the open drain for the hub heartbeat, or nil when no
// upgrade is being held. The hub's orphan sweep otherwise cannot tell a
// deliberately draining spoke from one whose attempt died: both are alive,
// heartbeating and still on the old SHA. A released drain reports nil too —
// the process is about to exit and the hub sees the restart directly. The
// same staleness rule as Active applies, so a drain the hub stopped asking
// about is not reported either.
func (d *upgradeDrain) Report(now time.Time) *spoke.UpgradeDrainReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released || d.target == "" || d.startedAt.IsZero() {
		return nil
	}
	if now.Sub(d.checkedAt) >= selfUpgradeDrainStaleAfter {
		return nil
	}
	waited := now.Sub(d.startedAt)
	remaining := d.timeout - waited
	if remaining < 0 {
		remaining = 0
	}
	rep := &spoke.UpgradeDrainReport{
		Target:     d.target,
		InFlight:   append([]string(nil), d.inFlight...),
		WaitedS:    int(waited / time.Second),
		RemainingS: int(remaining / time.Second),
	}
	for _, name := range d.inFlight {
		turn := spoke.UpgradeTurn{Agent: name}
		if at, ok := d.kickAt[name]; ok && !at.IsZero() && now.After(at) {
			turn.ElapsedS = int(now.Sub(at) / time.Second)
		}
		rep.Turns = append(rep.Turns, turn)
	}
	return rep
}

// setKickTimes records when the in-flight agents' turns started. Called next
// to decide with the same statuses it classified; a drain that is not open
// ignores it.
func (d *upgradeDrain) setKickTimes(kickAt map[string]time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released || d.target == "" {
		d.kickAt = nil
		return
	}
	d.kickAt = kickAt
}

// agentKickTimes returns the kick time of each named agent, for
// upgradeDrain.setKickTimes.
func agentKickTimes(statuses map[string]*crux.Process, names []string) map[string]time.Time {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]time.Time, len(names))
	for _, name := range names {
		if proc := statuses[name]; proc != nil && proc.LastKick != nil {
			out[name] = *proc.LastKick
		}
	}
	return out
}

// releaseLocked closes the drain window and latches the kick hold until the
// process exits.
func (d *upgradeDrain) releaseLocked(target string, now time.Time) {
	d.target = target
	d.startedAt = time.Time{}
	d.checkedAt = now
	d.inFlight, d.timeout = nil, 0
	d.kickAt = nil
	d.released = true
}

// wedgedKick is a kick agentKicksInFlight stopped counting as in flight
// because it has gone unsettled for longer than the agent's turn bound.
type wedgedKick struct {
	Agent   string
	KickAt  time.Time
	Age     time.Duration
	Ceiling time.Duration
}

// inFlightKickCeiling is the age past which an unsettled kick no longer holds
// the drain: the agent's continuous_turn_ceiling (the same bound continuous
// mode uses to presume a silent turn over), or DefaultContinuousTurnCeiling
// when the agent is unknown or the ceiling is disabled. The drain needs a
// bound either way — a turn-end signal that is never observed would otherwise
// hold every upgrade for the full drain window (#14323).
func inFlightKickCeiling(ceiling func(agent string) time.Duration, agent string) time.Duration {
	if ceiling != nil {
		if c := ceiling(agent); c > 0 {
			return c
		}
	}
	return config.DefaultContinuousTurnCeiling
}

// agentKicksInFlight names the running, unpaused agents whose most recent kick was
// delivered by THIS process and whose turn has not been classified as ended
// (crux.KickOutcome is settled by the pane poller / stall watchdog when the
// CLI returns to its prompt). Kicks seeded from persisted state predate
// processStart and cannot still be running here, so they are ignored. A kick
// left unsettled for longer than its agent's turn ceiling (see
// inFlightKickCeiling) is presumed wedged: it is returned in wedged instead of
// inFlight so a lost turn-end signal cannot hold the upgrade drain (#14323).
func agentKicksInFlight(statuses map[string]*crux.Process, processStart, now time.Time, ceiling func(agent string) time.Duration) (inFlight []string, wedged []wedgedKick) {
	for name, proc := range statuses {
		if proc == nil || proc.State != crux.StateRunning || proc.Paused || proc.LastKick == nil {
			continue
		}
		if !proc.LastKick.After(processStart) {
			continue
		}
		if proc.KickOutcome.Settled(proc.LastKick) {
			continue
		}
		bound := inFlightKickCeiling(ceiling, name)
		if age := now.Sub(*proc.LastKick); age >= bound {
			wedged = append(wedged, wedgedKick{Agent: name, KickAt: *proc.LastKick, Age: age, Ceiling: bound})
			continue
		}
		inFlight = append(inFlight, name)
	}
	sort.Strings(inFlight)
	sort.Slice(wedged, func(i, j int) bool { return wedged[i].Agent < wedged[j].Agent })
	return inFlight, wedged
}

// wedgedKickNotices remembers which wedged kicks were already logged, so the
// per-heartbeat upgrade check warns once per kick instead of every beat.
type wedgedKickNotices struct {
	mu     sync.Mutex
	logged map[string]time.Time
}

// selfUpgradeWedgedKicks is the process-wide wedged-kick log de-duplicator.
var selfUpgradeWedgedKicks = &wedgedKickNotices{}

// fresh returns the wedged kicks not reported before (keyed on agent and
// kick time, so a later kick that also wedges is reported again).
func (n *wedgedKickNotices) fresh(wedged []wedgedKick) []wedgedKick {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.logged == nil {
		n.logged = make(map[string]time.Time)
	}
	var out []wedgedKick
	for _, w := range wedged {
		if at, ok := n.logged[w.Agent]; ok && at.Equal(w.KickAt) {
			continue
		}
		n.logged[w.Agent] = w.KickAt
		out = append(out, w)
	}
	return out
}

// wedgedKickAgents names the agents in wedged, for log attributes.
func wedgedKickAgents(wedged []wedgedKick) []string {
	if len(wedged) == 0 {
		return nil
	}
	names := make([]string, 0, len(wedged))
	for _, w := range wedged {
		names = append(names, w.Agent)
	}
	return names
}

package agent

import "strings"

// MarkStartupLaunchQueued tells SendKick that the named agents are still in the
// boot stagger. A governor/manual kick that arrives before the agent reaches its
// pane is then held for delivery by the startup kick path instead of being
// refused as "stopped" and lost for the whole cadence interval.
func (m *Manager) MarkStartupLaunchQueued(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range names {
		if agent, ok := m.agents[name]; ok {
			agent.startupLaunchQueued = true
		}
	}
}

// GovernorMayStart reports whether the governor's kick path may start the named
// agent: it must be stopped and not stopped explicitly by an operator.
func (m *Manager) GovernorMayStart(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	return ok && agent.State == StateStopped && !agent.OperatorStopped
}

// maxDeferredMentionKicks bounds the per-agent deferred mention FIFO so a
// burst of mentions against an agent stuck in startup cannot grow unbounded.
const maxDeferredMentionKicks = 8

type deferredKick struct {
	message string
	source  string
}

// deferStartupKickLocked records a kick that arrives while startup owns
// delivery. Cadence/governor kicks coalesce: the latest one replaces the
// normal bootstrap prompt, since one readiness-gated kick should be typed into
// the fresh pane and the governor's actionable queue message is the fresher,
// more specific instruction. Mention/action-sourced kicks are different — each
// already carries a human-visible ack — so they are queued FIFO and never
// evicted by a later cadence kick (#12982). The startup delivery types the
// oldest queued mention unless a one-shot (non-cadence) kick is pending; the
// rest are delivered on later governor cadence kicks. Stop drops them (#13055).
//
// Caller holds m.mu.
func (m *Manager) deferStartupKickLocked(agent *AgentProcess, message, trigger string) bool {
	if agent.Paused || agent.State == StatePaused || agent.State == StateFailed {
		return false
	}
	startupKickInFlight := agent.startupKickInFlight && agent.startupKickGen == agent.launchGen
	if agent.startupKickInFlight && !startupKickInFlight {
		agent.startupKickInFlight = false
	}
	if !agent.startupLaunchQueued && !startupKickInFlight {
		return false
	}
	if mentionKickSource(trigger) {
		m.queueDeferredMentionKickLocked(agent, message, trigger)
	} else {
		agent.pendingStartupKick = message
		agent.pendingStartupSource = trigger
	}
	agent.KickRefused = true
	agent.KickRefusalReason = "deferred until startup completes"
	m.logger.Info("agent kick deferred until startup completes",
		"name", agent.Name,
		"state", agent.State,
		"trigger", trigger,
		"queued_mentions", len(agent.deferredMentionKicks),
		"startup_queued", agent.startupLaunchQueued,
		"startup_kick_in_flight", startupKickInFlight,
	)
	return true
}

// queueDeferredMentionKickLocked appends a mention/action kick to the agent's
// deferred FIFO. A re-sent kick for an already-queued source replaces its
// message in place. When the FIFO is full the oldest entry is genuinely
// dropped: that is audited and reported to the kick observer so the mention
// store does not keep a pending entry that will never be delivered.
//
// Caller holds m.mu.
func (m *Manager) queueDeferredMentionKickLocked(agent *AgentProcess, message, source string) {
	for i := range agent.deferredMentionKicks {
		if agent.deferredMentionKicks[i].source == source {
			agent.deferredMentionKicks[i].message = message
			return
		}
	}
	if len(agent.deferredMentionKicks) >= maxDeferredMentionKicks {
		dropped := agent.deferredMentionKicks[0]
		agent.deferredMentionKicks = agent.deferredMentionKicks[1:]
		m.logger.Warn("audit: deferred mention kick dropped",
			"name", agent.Name,
			"source", dropped.source,
			"reason", "deferred mention queue full",
			"queue_cap", maxDeferredMentionKicks,
		)
		m.notifyKickObserver(agent.Name, "kick-dropped", dropped.source)
	}
	agent.deferredMentionKicks = append(agent.deferredMentionKicks, deferredKick{message: message, source: source})
}

// startupKickTrigger is the trigger of the bootstrap prompt typed when an
// agent finishes starting.
const startupKickTrigger = "startup"

// nextDeferredMentionKickLocked substitutes the oldest queued mention kick for
// a kick that may safely yield to it: the governor cadence kick (re-sent on the
// next tick) or the startup bootstrap prompt. Every other kick — mentions
// themselves and one-shot messages from sourceless SendKick callers — is
// delivered as sent and the mention stays queued for the next cadence kick.
// fromQueue reports whether the returned kick must be removed from the FIFO
// once it is delivered.
//
// Caller holds m.mu.
func (m *Manager) nextDeferredMentionKickLocked(agent *AgentProcess, message, trigger string) (string, string, bool) {
	if len(agent.deferredMentionKicks) == 0 || (trigger != GovernorCadenceKickSource && trigger != startupKickTrigger) {
		return message, trigger, false
	}
	next := agent.deferredMentionKicks[0]
	if message != "" {
		m.logger.Info("cadence/startup kick superseded by queued mention kick",
			"name", agent.Name, "superseded_trigger", trigger, "trigger", next.source)
	}
	return next.message, next.source, true
}

// drainDeferredMentionKicksLocked drops every queued mention kick when the
// agent is stopped, so each mention's pending store entry and 👀 ack are
// released via kick-dropped instead of lingering indefinitely (#13055).
// Restarts keep the queue: the relaunched agent's startup delivery drains it.
//
// Caller holds m.mu.
func (m *Manager) drainDeferredMentionKicksLocked(agent *AgentProcess, reason string) {
	for _, dropped := range agent.deferredMentionKicks {
		m.logger.Warn("audit: deferred mention kick dropped",
			"name", agent.Name,
			"source", dropped.source,
			"reason", reason,
		)
		m.notifyKickObserver(agent.Name, "kick-dropped", dropped.source)
	}
	agent.deferredMentionKicks = nil
}

// removeDeferredMentionKickLocked removes a delivered kick from the FIFO by
// source rather than by index: m.mu is released while a kick is typed, so the
// queue may have changed underneath the delivery.
//
// Caller holds m.mu.
func removeDeferredMentionKickLocked(agent *AgentProcess, source string) {
	for i := range agent.deferredMentionKicks {
		if agent.deferredMentionKicks[i].source == source {
			agent.deferredMentionKicks = append(agent.deferredMentionKicks[:i], agent.deferredMentionKicks[i+1:]...)
			return
		}
	}
}

func mentionKickSource(source string) bool {
	source = strings.TrimSpace(source)
	return source == "mention" || strings.HasPrefix(source, "mention:") ||
		source == "action" || strings.HasPrefix(source, "action:")
}

func (m *Manager) clearStartupKickInFlight(agent *AgentProcess, gen int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.agents[agent.Name]; ok && current == agent && agent.startupKickGen == gen {
		agent.startupKickInFlight = false
	}
}

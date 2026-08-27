package agent

// Kick lifecycle observer (RFC #4492 Part 2, component D).
//
// The manager has no completion callback for a kick: a run "ends" only when
// the NEXT rotation point (a newer kick, a restart, a shutdown) archives its
// scrollback (#4296). External surfaces that need to narrate an agent's
// progress — the Linear AgentActivity emitter is the first — therefore hook
// exactly those two existing moments rather than inventing a new lifecycle:
//
//	"kick-delivered"     — deliverKickLocked put a kick into the pane
//	"kick-log-archived"  — a run's output was archived (the run is over)
//
// Same injection discipline as auditSink: an atomic.Pointer, NOT m.mu-guarded
// state, because both call sites run while m.mu is held and re-locking a
// non-reentrant RWMutex on the same goroutine deadlocks startup (see the
// isGatewayBackend comment in manager.go). The observer itself is invoked on
// a fresh goroutine so a slow consumer (an HTTP post to Linear) can never
// stall kick delivery or shutdown archiving.

// KickObserverEventDelivered / KickObserverEventArchived name the two events.
const (
	KickObserverEventDelivered = "kick-delivered"
	KickObserverEventArchived  = "kick-log-archived"
)

// SetKickObserver installs (or with nil, removes) the kick lifecycle
// observer. Safe to leave unset: notifications are then no-ops.
func (m *Manager) SetKickObserver(fn func(agentName, event, detail string)) {
	if fn == nil {
		m.kickObserver.Store(nil)
		return
	}
	m.kickObserver.Store(&fn)
}

// notifyKickObserver dispatches one event asynchronously. Callers may hold
// m.mu; the observer never runs under it.
func (m *Manager) notifyKickObserver(agentName, event, detail string) {
	fn := m.kickObserver.Load()
	if fn == nil {
		return
	}
	go (*fn)(agentName, event, detail)
}

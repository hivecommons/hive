package agent

import (
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
)

// KickObserverEventDelivered / KickObserverEventArchived name the two events
// the single-slot observer receives.
const (
	KickObserverEventDelivered = crux.KickEventDelivered
	KickObserverEventArchived  = crux.KickEventArchived
)

// kickObserverBuffer is how many undelivered events one fan-out observer may
// fall behind by before further events for it are dropped.
const kickObserverBuffer = 256

// Compile-time proof the in-process runtime offers fan-out observers.
var _ crux.KickObserverRegistry = (*Manager)(nil)

// SetKickObserver installs (or with nil, removes) the single-slot kick
// lifecycle observer. Safe to leave unset: notifications are then no-ops.
//
// This is the compatibility path (the Linear agent is its consumer): it
// receives only kick-delivered and kick-log-archived, each on its own
// goroutine, exactly as before AddKickObserver existed. It is independent of
// the fan-out observers.
func (m *Manager) SetKickObserver(fn func(agentName, event, detail string)) {
	if fn == nil {
		m.kickObserver.Store(nil)
		return
	}
	m.kickObserver.Store(&fn)
}

// kickObserverSet is the fan-out registry. Each observer owns a buffered
// channel drained by its own goroutine, so one observer sees events in the
// order they were emitted and a slow one never blocks the runtime or the
// others.
type kickObserverSet struct {
	mu   sync.Mutex
	next int
	subs map[int]chan crux.KickEvent
}

// AddKickObserver registers fn for every kick event (fan-out; see
// crux.KickObserverRegistry) and returns its remover. Calling the remover
// more than once is safe.
func (m *Manager) AddKickObserver(fn func(crux.KickEvent)) (remove func()) {
	if fn == nil {
		return func() {}
	}
	s := &m.kickObservers
	ch := make(chan crux.KickEvent, kickObserverBuffer)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = make(map[int]chan crux.KickEvent)
	}
	id := s.next
	s.next++
	s.subs[id] = ch
	s.mu.Unlock()

	go func() {
		for ev := range ch {
			fn(ev)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
			close(ch)
		})
	}
}

// emitKickEvent fans one event out to every registered observer without
// blocking. Safe to call with m.mu held.
func (m *Manager) emitKickEvent(ev crux.KickEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	s := &m.kickObservers
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
			if m.logger != nil {
				m.logger.Warn("kick observer is behind; event dropped",
					"agent", ev.Agent, "event", ev.Event, "kick_id", ev.KickID)
			}
		}
	}
}

// notifyKickObserver dispatches one delivered/archived event: to the
// single-slot observer on its own goroutine (unchanged), and to the fan-out
// observers with the kick ID of the agent's in-flight queued kick. Callers
// may hold m.mu; no observer runs under it.
func (m *Manager) notifyKickObserver(agentName, event, detail string) {
	if fn := m.kickObserver.Load(); fn != nil {
		go (*fn)(agentName, event, detail)
	}
	m.emitKickEvent(crux.KickEvent{
		Agent:  agentName,
		Event:  event,
		Detail: detail,
		KickID: m.InFlightKickID(agentName),
	})
}

// SetRelaunchObserver installs (or with nil, removes) the observer told when
// an agent's CLI is launched afresh; see Manager.relaunchObserver. Safe to
// leave unset.
func (m *Manager) SetRelaunchObserver(fn func(agentName string)) {
	if fn == nil {
		m.relaunchObserver.Store(nil)
		return
	}
	m.relaunchObserver.Store(&fn)
}

// notifyRelaunchObserver dispatches one relaunch asynchronously. Callers may
// hold m.mu; the observer never runs under it.
func (m *Manager) notifyRelaunchObserver(agentName string) {
	fn := m.relaunchObserver.Load()
	if fn == nil {
		return
	}
	go (*fn)(agentName)
}

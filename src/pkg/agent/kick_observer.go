package agent

import "strings"

// KickObserverEventDelivered / KickObserverEventArchived name the two events.
const (
	KickObserverEventDelivered = "kick-delivered"
	KickObserverEventArchived  = "kick-log-archived"
)

const kickObserverSourcePrefix = " source="

func kickObserverArchiveDetail(reason, source string) string {
	reason = strings.TrimSpace(reason)
	source = strings.TrimSpace(source)
	if source == "" {
		return reason
	}
	if reason == "" {
		return kickObserverSourcePrefix + source
	}
	return reason + kickObserverSourcePrefix + source
}

func KickObserverDetailSource(detail string) string {
	if _, source, ok := strings.Cut(detail, kickObserverSourcePrefix); ok {
		return strings.TrimSpace(source)
	}
	return ""
}

func KickObserverDetailReason(detail string) string {
	reason, _, _ := strings.Cut(detail, kickObserverSourcePrefix)
	return strings.TrimSpace(reason)
}

// SetKickObserver installs (or with nil, removes) the kick lifecycle
// observer. Safe to leave unset: notifications are then no-ops.
func (m *Manager) SetKickObserver(fn func(agentName, event, detail string)) {
	if fn == nil {
		m.kickObserver.Store(nil)
		return
	}
	if existing := m.kickObserver.Load(); existing != nil {
		prev := *existing
		chained := func(agentName, event, detail string) {
			prev(agentName, event, detail)
			fn(agentName, event, detail)
		}
		m.kickObserver.Store(&chained)
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

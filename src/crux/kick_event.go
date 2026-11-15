package crux

import "time"

// Kick lifecycle events delivered to fan-out kick observers (#18286). The
// first two are the events the single-slot SetKickObserver has always
// carried; the rest exist only on the fan-out path, so the legacy observer
// sees exactly what it saw before.
const (
	// KickEventDelivered: a kick prompt was typed into the agent. Detail is
	// the delivery trigger.
	KickEventDelivered = "kick-delivered"
	// KickEventArchived: the agent's kick log was archived. Detail is the
	// archive reason.
	KickEventArchived = "kick-log-archived"
	// KickEventQueued: a kick entered the agent's queue.
	KickEventQueued = "kick-queued"
	// KickEventCompleted: a queued kick's turn ended. Detail is the
	// KickOutcome kind.
	KickEventCompleted = "kick-completed"
	// KickEventFailed: a queued kick will not be delivered or its turn was
	// abandoned. Detail is the reason.
	KickEventFailed = "kick-failed"
)

// KickEvent is one kick lifecycle event as fan-out observers receive it.
type KickEvent struct {
	// Agent is the agent the event concerns.
	Agent string `json:"agent"`
	// Event is one of the KickEvent* constants.
	Event string `json:"event"`
	// Detail is event-specific; see the constants.
	Detail string `json:"detail,omitempty"`
	// KickID is the queued kick the event belongs to, or "" for a kick that
	// did not come through the kick queue.
	KickID string `json:"kickId,omitempty"`
	// At is when the runtime emitted the event.
	At time.Time `json:"at"`
}

// KickObserverRegistry is the optional runtime surface for fan-out kick
// observers. The in-process runtime (*agent.Manager) implements it; callers
// type-assert a crux.Runtime to it.
type KickObserverRegistry interface {
	// AddKickObserver registers fn for every kick event and returns a
	// function that unregisters it. Each observer receives events in order
	// on its own goroutine; an observer that falls far enough behind has
	// events dropped rather than stalling the runtime.
	AddKickObserver(fn func(KickEvent)) (remove func())
}

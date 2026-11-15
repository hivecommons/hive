package crux

// v6-only (hivecommons/hive#7483): v6 has two consumers of the
// (agentName, event, detail) kick callback, the mention responder and the
// Linear agent. Both register through AddLegacyKickObserver on the fan-out
// registry, so SetKickObserver stays the single-slot compatibility path it is
// on v5.

// kickEventDropped is the event a deferred kick that will never be delivered
// reports through the legacy callback (agent/kick_deferred.go).
const kickEventDropped = "kick-dropped"

// AddLegacyKickObserver registers fn, a callback in the SetKickObserver shape,
// alongside any other observer and returns its remover. fn receives exactly
// the events the single-slot observer receives (kick-delivered,
// kick-log-archived and kick-dropped) with the same agent, event and detail,
// in emission order, on the observer's own goroutine.
//
// A runtime without the fan-out registry (the sidecar client) has only the
// single slot: fn is installed with SetKickObserver, replacing any observer
// already there, and the remover is a no-op so it never clears another
// consumer's observer.
func AddLegacyKickObserver(rt Runtime, fn func(agentName, event, detail string)) (remove func()) {
	if rt == nil || fn == nil {
		return func() {}
	}
	reg, ok := rt.(KickObserverRegistry)
	if !ok {
		rt.SetKickObserver(fn)
		return func() {}
	}
	return reg.AddKickObserver(func(ev KickEvent) {
		switch ev.Event {
		case KickEventDelivered, KickEventArchived, kickEventDropped:
			fn(ev.Agent, ev.Event, ev.Detail)
		}
	})
}

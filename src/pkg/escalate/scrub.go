package escalate

import "github.com/hivecommons/hive/pkg/logscrub"

// scrubEvent passes every operator-visible field of an escalation through the
// shared secret/canary scrubber (`logscrub.ScrubString`) before it can leave
// the process.
//
// SECURITY: escalation bodies are assembled from agent-adjacent text — a
// review reason, a pause reason, a governor detail line — and push/on-call
// providers are third-party services outside the hive's trust boundary. A
// token or an ioscan canary that reaches ntfy, Pushover, or PagerDuty has
// left the boundary for good: it is in someone else's log, someone else's
// push-notification pipeline, and on an unlocked phone screen.
//
// Scrubbing used to happen only at the call sites in cmd/hive
// (escalation_runtime.go), which meant the guarantee held for exactly the
// three callers that remembered it and for no future one. Doing it here, at
// the surface boundary, makes it a property of the surface instead of a
// property of its callers — the v6 conformance invariant in
// src/docs/v6-readiness.md §2. ScrubString is idempotent, so the call sites
// that already scrub stay correct.
//
// Only the Event is scrubbed. Sink credentials (an ntfy bearer token, a
// Pushover app token, a PagerDuty routing key) are configuration, not event
// text: they are the authorization to deliver and must survive intact.
func scrubEvent(ev Event) Event {
	ev.Title = logscrub.ScrubString(ev.Title)
	ev.Body = logscrub.ScrubString(ev.Body)
	ev.Link = logscrub.ScrubString(ev.Link)
	return ev
}

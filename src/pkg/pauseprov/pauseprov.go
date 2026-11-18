// Package pauseprov says who or what paused an agent, when and why, and who or
// what last resumed it (hivecommons/hive#18916).
//
// A paused agent used to show only "Paused". The runtime already records the
// pause trigger, acting user, reason and time with the persisted pause state;
// this package turns that into an answer an operator can read ("paused by
// dashboard user alice at ...: manual pause", "auto-paused for provider rate
// limit or backoff ...", "unknown (before provenance was recorded)") and keeps
// the one thing the runtime does not: who or what last resumed the agent.
//
// Everything here is hive-side and read by the dashboard. Reasons are
// sanitized (tokens redacted, capped); none of it is sent to an agent.
package pauseprov

import (
	"strings"
	"time"
)

// Actor kinds.
const (
	// KindDashboardUser: a person paused or resumed from the dashboard.
	KindDashboardUser = "dashboard_user"
	// KindAutoRateLimit: the hive paused the agent for a provider rate limit,
	// quota or backoff (provider rotation found no headroom).
	KindAutoRateLimit = "auto_rate_limit"
	// KindAuto: another automatic safety or health trigger.
	KindAuto = "auto"
	// KindStartup: a pause applied at startup by design.
	KindStartup = "startup"
	// KindConfig: a pause set in configuration.
	KindConfig = "config"
	// KindHook: a hook paused the agent.
	KindHook = "hook"
	// KindUnknown: the pause predates provenance.
	KindUnknown = "unknown"
)

// UnknownLabel is the label of a pause or resume whose origin was not recorded.
const UnknownLabel = "unknown (before provenance was recorded)"

// Trigger words the runtime and the host stamp on a pause. They are code, not
// agent text.
const (
	triggerDashboard   = "dashboard-api"
	triggerRotation    = "provider-rotation"
	triggerStartup     = "startup"
	triggerStateRestor = "state-restore"
	triggerConfig      = "config"
	triggerHook        = "hook"
)

// maxTriggerRunes bounds a trigger word echoed in a label.
const maxTriggerRunes = 60

// Actor is who or what paused or resumed an agent.
type Actor struct {
	Kind string `json:"kind"`
	// Label is the readable form: "dashboard user alice", "auto-pause for
	// provider rate limit or backoff", "unknown (before provenance was
	// recorded)".
	Label string `json:"label"`
}

// Classify says who or what a trigger and acting user stand for.
func Classify(trigger, by string) Actor {
	trigger = strings.TrimSpace(trigger)
	by = quote(by)
	switch {
	case trigger == triggerDashboard:
		if by == "" {
			return Actor{KindDashboardUser, "a dashboard user"}
		}
		return Actor{KindDashboardUser, "dashboard user " + by}
	case trigger == "":
		return Actor{KindUnknown, UnknownLabel}
	case trigger == triggerStateRestor:
		// The pause was replayed at startup from persisted state that named
		// no trigger: restored, but its origin was never recorded.
		return Actor{KindStartup, "startup persisted pause, origin " + UnknownLabel}
	case trigger == triggerRotation:
		return Actor{KindAutoRateLimit, "auto-pause for provider rate limit or backoff"}
	case trigger == triggerStartup:
		return Actor{KindStartup, "startup (paused by design)"}
	case trigger == triggerConfig:
		return Actor{KindConfig, "configuration"}
	case trigger == triggerHook || strings.HasPrefix(trigger, triggerHook+":"):
		return Actor{KindHook, "hook (" + quote(trigger) + ")"}
	}
	t := quote(trigger)
	if r := []rune(t); len(r) > maxTriggerRunes {
		t = string(r[:maxTriggerRunes])
	}
	return Actor{KindAuto, "auto-pause (" + t + ")"}
}

// Event is one pause or resume as the host observes it.
type Event struct {
	Trigger string
	By      string
	Reason  string
	At      time.Time
}

// PauseFacts is what the runtime keeps about the current pause (the persisted
// pause state).
type PauseFacts struct {
	Trigger string
	By      string
	Reason  string
	At      time.Time
}

// Pause is the provenance of the current pause.
type Pause struct {
	Actor
	// At is when it was paused, RFC3339 UTC; empty when not recorded.
	At string `json:"at,omitempty"`
	// Reason is the stated reason, sanitized and capped.
	Reason string `json:"reason,omitempty"`
	// Line is "paused by <actor> at <time>: <reason>".
	Line string `json:"line"`
}

// Resume is the provenance of the last resume.
type Resume struct {
	Actor
	At     string `json:"at,omitempty"`
	Reason string `json:"reason,omitempty"`
	Line   string `json:"line"`
}

// Provenance is what the dashboard shows for an agent: the current pause, when
// there is one, and the last resume, when one is known.
type Provenance struct {
	Pause      *Pause  `json:"pause,omitempty"`
	LastResume *Resume `json:"lastResume,omitempty"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Describe builds the provenance for an agent: paused says whether it is
// paused now, facts what the runtime recorded about the pause, last the last
// resume the hive saw. It returns nil when there is nothing to say.
func Describe(paused bool, facts PauseFacts, last *Event) *Provenance {
	var p Provenance
	if paused {
		a := Classify(facts.Trigger, facts.By)
		pp := &Pause{Actor: a, At: stamp(facts.At), Reason: quote(facts.Reason)}
		pp.Line = "paused by " + a.Label
		if a.Kind == KindUnknown {
			pp.Line = "paused: " + a.Label
		}
		if pp.At != "" {
			pp.Line += " at " + pp.At
		}
		if pp.Reason != "" {
			pp.Line += ": " + pp.Reason
		}
		p.Pause = pp
	}
	if last != nil {
		a := Classify(last.Trigger, last.By)
		r := &Resume{Actor: a, At: stamp(last.At), Reason: quote(last.Reason)}
		r.Line = "last resumed by " + a.Label
		if a.Kind == KindUnknown {
			r.Line = "last resumed: " + a.Label
		}
		if r.At != "" {
			r.Line += " at " + r.At
		}
		p.LastResume = r
	}
	if p.Pause == nil && p.LastResume == nil {
		return nil
	}
	return &p
}

// RefusalMessage is the kick-refusal text for a paused agent: who or what
// paused it, when, why, and where to resume.
func RefusalMessage(agent string, p *Provenance) string {
	if p == nil || p.Pause == nil {
		return ""
	}
	return "agent " + agent + " cannot be kicked: it is " + p.Pause.Line + "; resume here"
}

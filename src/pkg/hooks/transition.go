// Package hooks implements state-triggered hooks as a first-class extension
// point (RFC #4001): operators declare, in the PVC-backed running config, that
// a named state transition should fire a vetted action.
//
// # Why this package exists
//
// Hive already reacts to state transitions everywhere — the governor's mode
// changes, agent pause/resume, sweep results, escalation's red-CI reactions,
// ACMM level changes, the #3836 upgrade kill switch. Every one of those
// reactions is hand-rolled at its call site, which is why the upgrade-pause
// file's own header has to warn that the pause must be honoured by every
// delivery path "or it is a lie". This package is where the NEXT such switch
// gets declared instead of threaded by hand.
//
// # Security posture (RFC #4001 maintainer position)
//
//   - Registration is operator-only, config-as-code. Hook definitions live in
//     the running config under the same authz and layer provenance as any other
//     config write. There is no runtime registration API and nothing here is
//     agent-writable: an agent that could register hooks on its own transitions
//     would have an escalation path.
//   - Hooks OBSERVE and NOTIFY freely, but MUTATE only through existing audited
//     APIs. No action in this package writes hive-state.json, the registry, or
//     any ledger directly; mutating actions call the same audited entry points
//     the dashboard uses, injected as narrow interfaces (see actions.go).
//   - DECLARATIVE ONLY. The action vocabulary is a closed, vetted set
//     (notify, pause, annotate, enqueue-approval). There is deliberately no
//     exec/script action in this slice — that is a separate RFC with its own
//     sandbox story. An unknown action is rejected at config-validation time,
//     fail-closed, exactly as pkg/celtrigger rejects a malformed predicate.
//   - Hooks fire POST-DURABLE-COMMIT, never inline in the critical path. See
//     Dispatcher.
//   - Loop prevention is structural: depth-1 causation, so a hook-caused
//     transition can never itself fire hooks. See Causation.
//
// This file owns the TRANSITION CATALOG: the closed, named set of transitions
// a hook may attach to, plus the typed payload each one carries.
package hooks

import (
	"sort"
	"strings"
)

// Transition is the name of a hookable state transition. The set is closed:
// Registry validation rejects any `on:` value that is not in the catalog, so a
// typo in operator config fails at load time rather than silently never firing
// — the single most common way a declarative rules engine lies to its user.
type Transition string

// The transition catalog. Each constant names one durable state transition
// hive already performs; the comment records where it originates so the
// emitter and the hook stay traceable to each other.
//
// ADDING A TRANSITION: declare the constant here, add a catalogEntry to
// catalog below (name, doc, and the payload fields it populates), and call
// Dispatcher.Fire at the emitting site AFTER the durable commit. Nothing else
// needs to change — the registry, CEL predicates, and every action work off
// the generic Payload. That cheapness is the point of the RFC.
const (
	// TransitionGovernorModeChange fires when the governor commits a mode
	// change (governor.ModeChange, appended to modeHistory).
	TransitionGovernorModeChange Transition = "governor_mode_change"

	// TransitionAgentPaused fires when an agent's paused flag is durably set,
	// whatever the trigger (operator, governor cadence, the login detector).
	// Payload.Trigger carries the paused_trigger provenance (#4041).
	TransitionAgentPaused Transition = "agent_paused"

	// TransitionAgentResumed fires when an agent's paused flag is durably
	// cleared.
	TransitionAgentResumed Transition = "agent_resumed"

	// TransitionSweepCompleted fires when an auto-merge sweep finishes and its
	// result is recorded (github.AutoMergeSweepResult).
	TransitionSweepCompleted Transition = "sweep_completed"

	// TransitionEscalationRed fires when escalation observes a red-CI state it
	// reacts to (escalation.ObserveRed).
	TransitionEscalationRed Transition = "escalation_red"

	// TransitionACMMLevelChange fires when the ACMM autonomy level is durably
	// changed via the audited pack-set-level path.
	TransitionACMMLevelChange Transition = "acmm_level_change"

	// TransitionUpgradePause fires on the #3836 upgrade kill switch flip — the
	// hand-rolled prototype of exactly this mechanism. Payload.To is "on" or
	// "off".
	TransitionUpgradePause Transition = "upgrade_pause"

	// TransitionReviewRejected fires when a human judges a review's output too
	// low-quality and sends the work back. This is the first hook shipped
	// end-to-end (RFC #4001's bluefin fleet-owner ask): the payload carries the
	// PRODUCING agent's backend/model/pin metadata so a notification can
	// deep-link the model-pin knob in the moment the quality problem is seen,
	// rather than making the owner hunt for it in the admin UI.
	TransitionReviewRejected Transition = "review_rejected"
)

// catalogEntry documents one transition for validation, docs generation, and
// the operator-facing error message produced when an unknown `on:` is used.
type catalogEntry struct {
	// Name is the transition as written in config.
	Name Transition
	// Doc is a one-line description of when the transition fires.
	Doc string
	// Fields lists the Payload fields this transition reliably populates,
	// which is what an operator needs to know to write a `when:` predicate.
	Fields []string
}

// catalog is the closed set of hookable transitions, keyed by name.
var catalog = map[Transition]catalogEntry{
	TransitionGovernorModeChange: {
		Name:   TransitionGovernorModeChange,
		Doc:    "The governor committed a mode change (e.g. normal→conserve).",
		Fields: []string{"from", "to", "reason"},
	},
	TransitionAgentPaused: {
		Name:   TransitionAgentPaused,
		Doc:    "An agent was durably paused. trigger carries the paused_trigger provenance.",
		Fields: []string{"agent", "trigger", "reason"},
	},
	TransitionAgentResumed: {
		Name:   TransitionAgentResumed,
		Doc:    "An agent was durably resumed.",
		Fields: []string{"agent", "trigger", "reason"},
	},
	TransitionSweepCompleted: {
		Name:   TransitionSweepCompleted,
		Doc:    "An auto-merge sweep completed and recorded its result.",
		Fields: []string{"repo", "reason", "attrs.merged", "attrs.skipped"},
	},
	TransitionEscalationRed: {
		Name:   TransitionEscalationRed,
		Doc:    "Escalation observed a red-CI state it reacts to.",
		Fields: []string{"repo", "agent", "reason", "attrs.pr"},
	},
	TransitionACMMLevelChange: {
		Name:   TransitionACMMLevelChange,
		Doc:    "The ACMM autonomy level changed via the audited pack-set-level path.",
		Fields: []string{"from", "to", "actor"},
	},
	TransitionUpgradePause: {
		Name:   TransitionUpgradePause,
		Doc:    "The #3836 upgrade kill switch flipped. to is \"on\" or \"off\".",
		Fields: []string{"to", "actor", "reason"},
	},
	TransitionReviewRejected: {
		Name: TransitionReviewRejected,
		Doc:  "A human rejected a review's output as low quality and sent the work back.",
		Fields: []string{
			"agent", "repo", "actor", "reason",
			"model", "backend", "pin", "acmm_level", "attrs.pr", "attrs.model_knob_url",
		},
	},
}

// KnownTransitions returns the catalog's transition names in sorted order.
// Used by config validation's error text and by the docs generator so the
// documented set can never drift from the enforced set.
func KnownTransitions() []Transition {
	out := make([]Transition, 0, len(catalog))
	for name := range catalog {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Describe returns the catalog entry for a transition and whether it is known.
func Describe(t Transition) (catalogEntry, bool) {
	e, ok := catalog[t]
	return e, ok
}

// IsKnownTransition reports whether t is in the catalog. Registry validation
// uses this to fail closed on an unknown `on:` value.
func IsKnownTransition(t Transition) bool {
	_, ok := catalog[t]
	return ok
}

// transitionList renders the catalog for an operator-facing error message.
func transitionList() string {
	names := KnownTransitions()
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, string(n))
	}
	return strings.Join(parts, ", ")
}

// Payload is the typed context a transition carries to its hooks. It is a
// single flat struct rather than a per-transition interface deliberately: the
// CEL `when:` predicate, the audit record, and every action work off ONE shape,
// which is what keeps adding a transition to a constant + a catalog entry.
//
// Fields not meaningful for a given transition are left zero; the catalog's
// Fields list documents which are populated where. Attrs carries anything
// transition-specific that does not deserve a typed field.
type Payload struct {
	// Transition is the catalog name. Always set by Fire.
	Transition Transition `json:"transition"`

	// From and To describe a state change (governor mode, ACMM level,
	// upgrade-pause on/off).
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`

	// Agent is the agent the transition concerns, when applicable.
	Agent string `json:"agent,omitempty"`
	// Repo is the repository the transition concerns, e.g. "org/name".
	Repo string `json:"repo,omitempty"`
	// Actor is the responsible user, or "system" for hive-originated
	// transitions. Mirrors the audit sink's actor convention.
	Actor string `json:"actor,omitempty"`
	// Trigger is the provenance of the transition (operator, governor,
	// login_detector, …). For pause/resume this is the paused_trigger value.
	Trigger string `json:"trigger,omitempty"`
	// Reason is a short human-readable explanation.
	Reason string `json:"reason,omitempty"`

	// Model, Backend, and Pin identify the model that PRODUCED the artifact the
	// transition concerns. These are the fields that make review_rejected
	// actionable: the notification can name the stale pin and link its knob.
	// Names follow pkg/tracing/semconv.go's vocabulary.
	Model   string `json:"model,omitempty"`
	Backend string `json:"backend,omitempty"`
	Pin     string `json:"pin,omitempty"`
	// ACMMLevel is the autonomy level in effect, 0 when unknown.
	ACMMLevel int `json:"acmm_level,omitempty"`

	// At is the transition time in Unix milliseconds. Fire stamps it when zero.
	At int64 `json:"at,omitempty"`

	// Attrs carries free-form transition-specific context. Exposed to CEL as
	// a string map so predicates can read it without a schema change here.
	Attrs map[string]string `json:"attrs,omitempty"`

	// Causation records what caused this transition, and is what makes the
	// depth-1 guard work. Never set this by hand at an emitting site — Fire
	// manages it, and an action that causes a further transition propagates it
	// via Causation.Child.
	Causation Causation `json:"causation"`
}

// attr returns an Attrs value, tolerating a nil map.
func (p Payload) attr(key string) string {
	if p.Attrs == nil {
		return ""
	}
	return p.Attrs[key]
}

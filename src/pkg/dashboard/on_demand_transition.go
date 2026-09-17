package dashboard

// onDemandAction is what a change to an agent's on_demand flag requires of the
// running process. Toggling the flag is a config edit, but "on demand" is a
// statement about whether a process should EXIST, so the two have to be kept in
// agreement (#7446).
type onDemandAction int

const (
	// onDemandNoop covers every non-transition, including a save that did not
	// touch on_demand at all. The settings dialog PUTs only dirty keys, so most
	// saves land here.
	onDemandNoop onDemandAction = iota

	// onDemandStart: the agent left on-demand and must begin running now.
	// Nothing else starts it — it was skipped at launch precisely because it
	// was on-demand, and ReconcileAgents only reports newly ADDED agents, never
	// an existing one whose flag changed.
	onDemandStart

	// onDemandStop: the agent became on-demand and must stop. The governor
	// already declines to kick on-demand agents, so leaving the process up
	// would strand a live CLI and pane that can never be scheduled again.
	onDemandStop
)

// onDemandTransition maps an on_demand change to the process action it implies.
//
// Only a genuine edge acts. Re-saving the same value is deliberately a no-op,
// so an operator editing an unrelated field on an agent's settings page never
// restarts or kills it as a side effect.
func onDemandTransition(prev, next, enabled bool) onDemandAction {
	switch {
	case prev == next:
		return onDemandNoop
	case !next:
		// Leaving on-demand. A disabled agent must stay down: "not on demand"
		// describes how it would be scheduled if it ran at all, and enabling it
		// is a separate decision the operator has not made.
		if !enabled {
			return onDemandNoop
		}
		return onDemandStart
	default:
		// Becoming on-demand. This applies even to a disabled agent, where
		// stopping is a harmless no-op, so the rule needs no exception.
		return onDemandStop
	}
}

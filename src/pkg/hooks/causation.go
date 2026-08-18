package hooks

// Causation records why a transition happened, and is the mechanism that makes
// hook→transition→hook loops structurally impossible rather than merely
// unlikely.
//
// # The loop
//
// `pause` is both an ACTION and a TRANSITION. A hook on agent_paused whose
// action is pause therefore feeds itself; so does any longer cycle
// (escalation_red → pause → agent_paused → notify → …). Rate limiting alone
// does not fix this — it only slows the storm down, and a slow infinite loop is
// still an infinite loop that fills the audit log.
//
// # The guard
//
// Depth 1, as specified in RFC #4001: a transition CAUSED BY A HOOK does not
// itself fire hooks. Every payload carries its Depth; Fire refuses to dispatch
// when Depth > 0. Because the guard is checked in Fire — the single entry point
// — no emitting site can forget it, and no action can opt out of it.
//
// Depth 1 rather than a configurable cap is deliberate for this slice. A
// configurable depth invites an operator to raise it, and the useful cases
// (notify on a state change, enqueue an approval) are all depth-0 → depth-1.
// Genuine multi-stage reactions should be expressed as multiple hooks on real
// transitions, which stays auditable, instead of a chain that is hard to reason
// about and easy to make cyclic.
type Causation struct {
	// Depth is 0 for a transition caused by the world (an operator, the
	// governor, CI) and 1 for one caused by a hook action. Fire will not
	// dispatch hooks for a payload with Depth > 0.
	Depth int `json:"depth"`

	// HookName is the hook whose action caused this transition, empty at
	// depth 0. It makes the audit trail answer "why did this agent pause?"
	// with a config rule name rather than just "system".
	HookName string `json:"hookName,omitempty"`

	// OriginTransition is the transition at the root of the chain, empty at
	// depth 0. Together with HookName it is the full causal story for a
	// depth-1 transition.
	OriginTransition Transition `json:"originTransition,omitempty"`
}

// maxHookDepth is the hard causation cap. Transitions at or above this depth do
// not fire hooks. Named rather than inlined as `> 0` so the guard reads as the
// deliberate policy it is, and so raising it later is a single, reviewable
// edit rather than a hunt for bare integer comparisons.
const maxHookDepth = 1

// IsHookCaused reports whether this transition was produced by a hook action
// rather than by the world.
func (c Causation) IsHookCaused() bool { return c.Depth > 0 }

// MayFireHooks reports whether a transition with this causation is allowed to
// dispatch hooks. This is the depth-1 guard, and Fire is its only caller.
func (c Causation) MayFireHooks() bool { return c.Depth < maxHookDepth }

// Child returns the causation an action must attach to any transition it
// causes, so the resulting transition is correctly marked hook-caused and
// cannot cascade.
//
// Actions that mutate state through an audited API (pause, enqueue-approval)
// pass this down so the downstream transition — which the audited API will
// itself emit — carries the chain. An action that neglects to do so would
// produce a depth-0 transition and reopen the loop, which is why the mutating
// actions take a Causation parameter rather than deriving one.
func (c Causation) Child(hookName string, origin Transition) Causation {
	// The origin is preserved from the root of the chain when one already
	// exists, so a depth-1 transition names the real cause and not its
	// immediate predecessor.
	rootOrigin := c.OriginTransition
	if rootOrigin == "" {
		rootOrigin = origin
	}
	return Causation{
		Depth:            c.Depth + 1,
		HookName:         hookName,
		OriginTransition: rootOrigin,
	}
}

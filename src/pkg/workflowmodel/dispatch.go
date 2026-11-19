package workflowmodel

import (
	"strings"
)

// Agent dispatch (hivecommons/hive#17620 follow-up, cncf/techdocs#395).
//
// A model's agent grants (spec.agents) name the profiles that may act in
// which states. The compiler resolves, for each grant, the states in which
// the hive hands an item to that profile and what starts each hand-off:
//
//   - explicit: the grant's dispatch block (states, reply, instructions);
//   - convention, when the grant has no dispatch block: the grant's FIRST
//     state is its entry state. A profile that produces a pull request or a
//     push is dispatched once on entering it (the TechDocs drafter on
//     Accepted). A profile that produces neither (a verifier, a reviewer)
//     is dispatched on the reply that releases the gate out of its entry
//     state into a non-terminal state, when that state declares the reply
//     (the TechDocs verifier on `/verify` in Drafting).
//
// The compiled Profile carries the result as Dispatch (state id →
// DispatchTarget). Which hive agent fills a profile is the hive's
// configuration (config.WorkflowConfig.Dispatch), not the model's: a model
// names roles, a hive names agents. The built-in hive-default model never
// dispatches; its grants describe the fleet that already runs it.

// hiveDefaultModelName is the built-in model (hivedefault.Name), whose
// grants are descriptive only.
const hiveDefaultModelName = "hive-default"

// DispatchTarget is one state in which the hive hands an item to a profile.
type DispatchTarget struct {
	// Profile is the agent grant's profile name.
	Profile string `json:"profile"`
	// State is the model state id the target applies to.
	State string `json:"state"`
	// StateLabel is the state's display label.
	StateLabel string `json:"stateLabel,omitempty"`
	// Kinds are the item kinds the state applies to (empty: both).
	Kinds []ItemKind `json:"kinds,omitempty"`
	// Reply, when set, is the human reply that starts each pass; empty
	// dispatches once on entering the state.
	Reply string `json:"reply,omitempty"`
	// Explicit is true when the grant declared a dispatch block; false when
	// the convention resolved the target.
	Explicit bool `json:"explicit,omitempty"`
	// Tools and Produces are the grant's, for the prompt.
	Tools    []string `json:"tools,omitempty"`
	Produces []string `json:"produces,omitempty"`
	// Instructions is the state's guidance for the profile: the grant's
	// dispatch instructions, the state's waiting reasons and its column
	// hint, in that order.
	Instructions []string `json:"instructions,omitempty"`
	// Deliverable is the pull request the profile is expected to open, when
	// it produces one and a later grant state is a pull-request state.
	Deliverable *Deliverable `json:"deliverable,omitempty"`
}

// Deliverable is the pull request a dispatched profile opens: the model
// state it should land in and the labels and draft flag that state's
// derivation rule requires.
type Deliverable struct {
	State  string   `json:"state"`
	Draft  bool     `json:"draft,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// ProducesChange reports whether the profile opens a pull request or
// pushes in this state. A target that does not is a reviewing profile: it
// reports on the item (TechDocs' techdocs-verifier).
func (t DispatchTarget) ProducesChange() bool { return producesChange(t.Produces) }

// AppliesTo reports whether the target's state applies to items of kind.
func (t DispatchTarget) AppliesTo(kind ItemKind) bool {
	if len(t.Kinds) == 0 {
		return true
	}
	for _, k := range t.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// DispatchFor returns the dispatch target for state, if the model hands
// items in that state to a profile.
func (p *Profile) DispatchFor(state string) (DispatchTarget, bool) {
	if p == nil || p.Dispatch == nil || state == "" {
		return DispatchTarget{}, false
	}
	t, ok := p.Dispatch[state]
	return t, ok
}

// Dispatches reports whether the profile hands any state to an agent.
func (p *Profile) Dispatches() bool { return p != nil && len(p.Dispatch) > 0 }

// producesChange reports whether a grant's outputs include a pull request
// or a push: such a profile is dispatched on entry by convention.
func producesChange(produces []string) bool {
	for _, p := range produces {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "pull-request", "pr", "push":
			return true
		}
	}
	return false
}

// dispatch resolves every grant's dispatch targets onto the profile.
func (c *compiler) dispatch() {
	m := c.m
	c.p.Agents = append([]AgentGrant(nil), m.Spec.Agents...)
	if m.Metadata.Name == hiveDefaultModelName || len(m.Spec.Agents) == 0 {
		return
	}
	gates := map[string]Gate{}
	for _, g := range m.Spec.Gates {
		gates[g.ID] = g
	}
	for _, a := range m.Spec.Agents {
		if len(a.States) == 0 {
			continue
		}
		grantStates := map[string]bool{}
		for _, s := range a.States {
			grantStates[s] = true
		}
		var states []string
		reply, explicit, instr := "", a.Dispatch != nil, ""
		if explicit {
			for _, s := range a.Dispatch.States {
				if !grantStates[s] {
					c.errorf(CodeUnknownState, a.Profile, "agent %q dispatches in %q, which is not one of its states", a.Profile, s)
					continue
				}
				states = append(states, s)
			}
			reply, instr = strings.TrimSpace(a.Dispatch.Reply), strings.TrimSpace(a.Dispatch.Instructions)
		} else {
			entry := a.States[0]
			if _, ok := c.p.States[entry]; !ok {
				continue // roles() already refused the unknown state
			}
			states = []string{entry}
			if !producesChange(a.Produces) {
				reply = c.exitGateReply(entry, gates)
			}
		}
		for _, sid := range states {
			st, ok := c.p.States[sid]
			if !ok {
				continue
			}
			if prev, dup := c.p.Dispatch[sid]; dup {
				c.errorf(CodeDuplicateDispatch, sid, "state %q is dispatched to both %q and %q", sid, prev.Profile, a.Profile)
				continue
			}
			if reply != "" && !stateDeclaresReply(st, reply) {
				c.warnf(CodeDispatchReply, sid, "agent %q is dispatched on %q, which state %q does not declare as a reply", a.Profile, reply, sid)
			}
			t := DispatchTarget{
				Profile:    a.Profile,
				State:      sid,
				StateLabel: st.Label,
				Kinds:      append([]ItemKind(nil), st.Lifecycle.Kinds...),
				Reply:      reply,
				Explicit:   explicit,
				Tools:      append([]string(nil), a.Tools...),
				Produces:   append([]string(nil), a.Produces...),
			}
			if instr != "" {
				t.Instructions = append(t.Instructions, instr)
			}
			t.Instructions = append(t.Instructions, st.Lifecycle.Waiting...)
			if col := c.p.Column(c.p.ColumnOf[sid]); col != nil && col.Hint != "" {
				t.Instructions = append(t.Instructions, col.Hint)
			}
			if producesChange(a.Produces) {
				t.Deliverable = c.deliverable(a.States, sid)
			}
			if c.p.Dispatch == nil {
				c.p.Dispatch = map[string]DispatchTarget{}
			}
			c.p.Dispatch[sid] = t
		}
	}
}

// exitGateReply is the reply of the first human gate on a transition out
// of state into a non-terminal state, when state declares that reply; ""
// when there is none. It is what a reviewing profile waits for.
func (c *compiler) exitGateReply(state string, gates map[string]Gate) string {
	st := c.p.States[state]
	for _, t := range c.m.Spec.Transitions {
		if t.From != state || t.GateRef == "" || c.p.Terminal[t.To] {
			continue
		}
		g, ok := gates[t.GateRef]
		if !ok || strings.TrimSpace(g.Reply) == "" {
			continue
		}
		if stateDeclaresReply(st, g.Reply) {
			return strings.TrimSpace(g.Reply)
		}
	}
	return ""
}

// stateDeclaresReply reports whether st offers reply (NormalizeReply-equal,
// a leading slash ignored).
func stateDeclaresReply(st *State, reply string) bool {
	if st == nil {
		return false
	}
	want := NormalizeReply(strings.TrimPrefix(strings.TrimSpace(reply), "/"))
	for _, r := range st.Lifecycle.Replies {
		if NormalizeReply(strings.TrimPrefix(strings.TrimSpace(r), "/")) == want {
			return true
		}
	}
	return false
}

// deliverable is the first pull-request state among the grant's states
// after from, with the labels and draft flag its first derivation rule
// requires. Nil when the grant names no pull-request state.
func (c *compiler) deliverable(grantStates []string, from string) *Deliverable {
	for _, sid := range grantStates {
		if sid == from {
			continue
		}
		st := c.p.States[sid]
		if st == nil || !stateKind(st, KindPR) {
			continue
		}
		d := &Deliverable{State: sid}
		for _, r := range c.m.Spec.Derivation.Rules {
			if r.State != sid || (r.Kind != KindPR && r.Kind != KindAny) {
				continue
			}
			for _, pred := range r.When {
				switch {
				case pred.Fact == "draft" && pred.Op == OpTrue:
					d.Draft = true
				case pred.Fact == "labels" && pred.Op == OpHas && pred.Value != "" && !strings.ContainsAny(pred.Value, "*?["):
					d.Labels = append(d.Labels, pred.Value)
				}
			}
			break
		}
		return d
	}
	return nil
}

// stateKind reports whether st applies to items of kind (no kinds: both).
func stateKind(st *State, kind ItemKind) bool {
	if len(st.Lifecycle.Kinds) == 0 {
		return true
	}
	for _, k := range st.Lifecycle.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

package workflowmodel

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Diagnostic is one compiler finding. Errors refuse the model; warnings
// are shown in the editor and on the monitor but compile.
type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Subject  string `json:"subject,omitempty"`
	Detail   string `json:"detail"`
}

// Diagnostic severities.
const (
	DiagError   = "error"
	DiagWarning = "warning"
)

// Diagnostic codes. Each is one of the refusals in hivecommons/hive#17620
// "Deterministic", or a structural check the engine needs.
const (
	CodeNoExit           = "state-no-exit"
	CodeGateNoOwner      = "gate-no-owner"
	CodeAgentNoHumanGate = "agent-no-human-gate"
	CodeDuplicateEvent   = "duplicate-event-fact"
	CodeNoColumn         = "state-no-column"
	CodeUnknownColumn    = "unknown-column"
	CodeUnknownState     = "unknown-state"
	CodeColumnEmpty      = "column-empty"
	CodeNoTerminalColumn = "no-terminal-column"
	CodeDuplicateNumber  = "duplicate-stage-number"
	CodeUnknownFact      = "unknown-fact"
	CodeBadOp            = "bad-op"
	CodeNoRules          = "no-rules"
	CodeBadMode          = "bad-derivation-mode"
	CodeUnknownRole      = "unknown-role"
	CodeUnknownGate      = "unknown-gate"
	CodeUnknownEvent     = "unknown-event"
	CodeNoVersion        = "no-version"
	CodeInferred         = "inferred-element"
)

// ErrCompile wraps a compile refused by error diagnostics.
var ErrCompile = errors.New("workflow model does not compile")

// Profile is a compiled model: what the engine and the monitor consume.
// It is derived, never edited; recompile from the Model instead.
type Profile struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hash    string `json:"hash"`
	// Mode is the derivation mode.
	Mode DerivationMode `json:"mode"`
	// Columns in display order with their states resolved.
	Columns []Column `json:"columns"`
	// States by id, flattened (regions included).
	States map[string]*State `json:"-"`
	// ColumnOf maps a state id to its column id.
	ColumnOf map[string]string `json:"columnOf"`
	// StateOfNumber maps a stage number to the state id that carries it.
	StateOfNumber map[int]string `json:"stateOfNumber"`
	// Terminal is the set of states in a terminal column or of final type.
	Terminal map[string]bool `json:"terminal"`
	// Rules are the derivation rules, in evaluation order (rules mode).
	Rules []Rule `json:"rules,omitempty"`
	// Replies maps a state id to the human-gate verbs it offers.
	Replies map[string][]string `json:"replies,omitempty"`
	// Labels is the label set to reconcile.
	Labels LabelSet `json:"labels,omitzero"`
	// Diagnostics are the warnings left after a successful compile.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// replyEffects maps state id → NormalizeReply(verb) → the transition
	// the verb releases (replies.go). Derived; never serialised.
	replyEffects map[string]map[string]ReplyEffect
}

// Compile validates m, then checks the lifecycle half and builds the
// Profile. Error diagnostics refuse the compile: the returned Profile is
// nil and err wraps ErrCompile; diagnostics are always returned so an
// editor can show them all at once.
func Compile(m *Model) (*Profile, []Diagnostic, error) {
	if err := m.Validate(); err != nil {
		return nil, []Diagnostic{{Code: "invalid", Severity: DiagError, Detail: err.Error()}}, err
	}
	c := &compiler{m: m}
	c.run()
	sort.SliceStable(c.diags, func(i, j int) bool {
		if c.diags[i].Severity != c.diags[j].Severity {
			return c.diags[i].Severity == DiagError
		}
		return false
	})
	for _, d := range c.diags {
		if d.Severity == DiagError {
			return nil, c.diags, fmt.Errorf("%w: %s", ErrCompile, summarize(c.diags))
		}
	}
	c.p.Diagnostics = c.diags
	return c.p, c.diags, nil
}

func summarize(diags []Diagnostic) string {
	var parts []string
	for _, d := range diags {
		if d.Severity != DiagError {
			continue
		}
		if d.Subject != "" {
			parts = append(parts, d.Code+" "+d.Subject)
		} else {
			parts = append(parts, d.Code)
		}
	}
	return strings.Join(parts, "; ")
}

type compiler struct {
	m     *Model
	p     *Profile
	diags []Diagnostic
}

func (c *compiler) errorf(code, subject, format string, a ...any) {
	c.diags = append(c.diags, Diagnostic{Code: code, Severity: DiagError, Subject: subject, Detail: fmt.Sprintf(format, a...)})
}

func (c *compiler) warnf(code, subject, format string, a ...any) {
	c.diags = append(c.diags, Diagnostic{Code: code, Severity: DiagWarning, Subject: subject, Detail: fmt.Sprintf(format, a...)})
}

func (c *compiler) run() {
	m := c.m
	c.p = &Profile{Name: m.Metadata.Name, Version: m.Spec.Meta.Version, Hash: m.Hash(), Mode: m.Spec.Derivation.Mode, States: map[string]*State{}, ColumnOf: map[string]string{}, StateOfNumber: map[int]string{}, Terminal: map[string]bool{}, Replies: map[string][]string{}, Labels: m.Spec.Labels}
	if c.p.Version == "" {
		c.errorf(CodeNoVersion, "", "spec.meta.version is empty")
	}
	var flatten func(states []State)
	flatten = func(states []State) {
		for i := range states {
			s := &states[i]
			c.p.States[s.ID] = s
			flatten(s.Regions)
		}
	}
	flatten(m.Spec.States)

	c.columns()
	c.numbers()
	c.replies()
	c.exits()
	c.roles()
	c.events()
	c.derivation()
	c.inferred()
}

// columns checks every top-level state sits in exactly one column and
// every column names real states; a terminal column must exist.
func (c *compiler) columns() {
	m := c.m
	cols := map[string]bool{}
	terminal := false
	for _, col := range m.Spec.Columns {
		cols[col.ID] = true
		if col.Terminal {
			terminal = true
		}
		if len(col.States) == 0 {
			c.errorf(CodeColumnEmpty, col.ID, "column %q collects no state", col.ID)
		}
		for _, sid := range col.States {
			s, ok := c.p.States[sid]
			if !ok {
				c.errorf(CodeUnknownState, col.ID, "column %q collects unknown state %q", col.ID, sid)
				continue
			}
			if prev, dup := c.p.ColumnOf[sid]; dup && prev != col.ID {
				c.errorf(CodeUnknownColumn, sid, "state %q is in columns %q and %q", sid, prev, col.ID)
			}
			c.p.ColumnOf[sid] = col.ID
			if col.Terminal || s.Type == StateFinal {
				c.p.Terminal[sid] = true
			}
		}
	}
	for _, s := range m.Spec.States {
		if s.Lifecycle.Column != "" && !cols[s.Lifecycle.Column] {
			c.errorf(CodeUnknownColumn, s.ID, "state %q names column %q, which does not exist", s.ID, s.Lifecycle.Column)
		}
		if s.Lifecycle.Column != "" && c.p.ColumnOf[s.ID] != "" && c.p.ColumnOf[s.ID] != s.Lifecycle.Column {
			c.errorf(CodeUnknownColumn, s.ID, "state %q says column %q but column %q collects it", s.ID, s.Lifecycle.Column, c.p.ColumnOf[s.ID])
		}
		if _, ok := c.p.ColumnOf[s.ID]; !ok {
			if s.Lifecycle.Column != "" && cols[s.Lifecycle.Column] {
				c.p.ColumnOf[s.ID] = s.Lifecycle.Column
				continue
			}
			c.errorf(CodeNoColumn, s.ID, "state %q is not in any column", s.ID)
		}
		if s.Type == StateFinal {
			c.p.Terminal[s.ID] = true
		}
	}
	if len(m.Spec.Columns) > 0 && !terminal {
		c.errorf(CodeNoTerminalColumn, "", "no column is terminal")
	}
	c.p.Columns = append([]Column(nil), m.Spec.Columns...)
}

// numbers checks stage numbers are unique per item kind.
func (c *compiler) numbers() {
	seen := map[string]string{}
	for _, s := range c.m.Spec.States {
		n := s.Lifecycle.Number
		if n == 0 {
			continue
		}
		kinds := s.Lifecycle.Kinds
		if len(kinds) == 0 {
			kinds = []ItemKind{KindIssue, KindPR}
		}
		for _, k := range kinds {
			key := fmt.Sprintf("%s/%d", k, n)
			if prev, dup := seen[key]; dup {
				c.errorf(CodeDuplicateNumber, s.ID, "states %q and %q both carry stage number %d for %s", prev, s.ID, n, k)
			}
			seen[key] = s.ID
		}
		if _, ok := c.p.StateOfNumber[n]; !ok {
			c.p.StateOfNumber[n] = s.ID
		}
		if len(s.Lifecycle.Replies) > 0 {
			c.p.Replies[s.ID] = append([]string(nil), s.Lifecycle.Replies...)
		}
	}
}

// exits checks every non-terminal state has a way out.
func (c *compiler) exits() {
	out := map[string]bool{}
	for _, t := range c.m.Spec.Transitions {
		out[t.From] = true
	}
	for _, s := range c.m.Spec.States {
		if c.p.Terminal[s.ID] || out[s.ID] || s.Type == StateParallel {
			continue
		}
		c.errorf(CodeNoExit, s.ID, "state %q is not terminal and has no transition out", s.ID)
	}
}

// roles checks gates name a releasing role and that an agent-granted
// state is followed by a human-released gate on every path out.
func (c *compiler) roles() {
	m := c.m
	roles := map[string]RoleDef{}
	for _, r := range m.Spec.Roles {
		roles[r.ID] = r
	}
	gates := map[string]Gate{}
	for _, g := range m.Spec.Gates {
		gates[g.ID] = g
		if g.ReleasedBy == "" {
			c.errorf(CodeGateNoOwner, g.ID, "gate %q has no releasing role", g.ID)
			continue
		}
		if _, ok := roles[g.ReleasedBy]; !ok {
			c.errorf(CodeUnknownRole, g.ID, "gate %q is released by unknown role %q", g.ID, g.ReleasedBy)
		}
	}
	for _, t := range m.Spec.Transitions {
		if t.GateRef != "" {
			if _, ok := gates[t.GateRef]; !ok {
				c.errorf(CodeUnknownGate, t.From+"->"+t.To, "transition %s->%s needs unknown gate %q", t.From, t.To, t.GateRef)
			}
		}
	}
	for _, e := range m.Spec.Events {
		if e.Actor != "" {
			if _, ok := roles[e.Actor]; !ok {
				c.errorf(CodeUnknownRole, e.ID, "event %q actor %q is not a role", e.ID, e.Actor)
			}
		}
	}
	if len(m.Spec.Agents) == 0 {
		return
	}
	humanGate := func(t Transition) bool {
		g, ok := gates[t.GateRef]
		return ok && roles[g.ReleasedBy].Human
	}
	// A closure edge (item closed unmerged) is an observation, not an
	// outcome an agent can bring about, so it is not a path to inspect.
	events := map[string]Event{}
	for _, e := range m.Spec.Events {
		events[e.ID] = e
	}
	closure := func(t Transition) bool {
		e, ok := events[t.On]
		return ok && c.p.Terminal[t.To] && e.Fact.Fact == "open" && e.Fact.Op == OpFalse
	}
	// adjacency over transitions
	next := map[string][]Transition{}
	for _, t := range m.Spec.Transitions {
		next[t.From] = append(next[t.From], t)
	}
	for _, a := range m.Spec.Agents {
		for _, sid := range a.States {
			if _, ok := c.p.States[sid]; !ok {
				c.errorf(CodeUnknownState, a.Profile, "agent %q acts in unknown state %q", a.Profile, sid)
				continue
			}
			if !c.humanGateReachable(sid, next, humanGate, closure) {
				c.errorf(CodeAgentNoHumanGate, sid, "agent %q acts in %q but no human-released gate follows it before a terminal state", a.Profile, sid)
			}
		}
	}
}

// humanGateReachable is true when every path from start to a terminal
// state crosses a human-released gate (a path with no such gate is a
// refusal). Cycles without a gate do not count as a path; closure edges
// are skipped.
func (c *compiler) humanGateReachable(start string, next map[string][]Transition, human, skip func(Transition) bool) bool {
	var visit func(state string, seen map[string]bool) bool
	visit = func(state string, seen map[string]bool) bool {
		if c.p.Terminal[state] {
			return false // reached terminal without a gate
		}
		if seen[state] {
			return true // cycle: no gate-free path to terminal through here
		}
		seen[state] = true
		defer delete(seen, state)
		for _, t := range next[state] {
			if human(t) || skip(t) {
				continue
			}
			if !visit(t.To, seen) {
				return false
			}
		}
		return true
	}
	return visit(start, map[string]bool{})
}

// events checks no two events bind the same predicate and transitions
// name declared events.
func (c *compiler) events() {
	m := c.m
	ids := map[string]bool{}
	byFact := map[string]string{}
	for _, e := range m.Spec.Events {
		ids[e.ID] = true
		c.predicate(e.ID, e.Fact)
		k := e.Fact.Fact + "|" + string(e.Fact.Op) + "|" + e.Fact.Value
		if prev, dup := byFact[k]; dup {
			c.errorf(CodeDuplicateEvent, e.ID, "events %q and %q both fire on %s %s %s", prev, e.ID, e.Fact.Fact, e.Fact.Op, e.Fact.Value)
		}
		byFact[k] = e.ID
	}
	if len(m.Spec.Events) == 0 {
		return
	}
	for _, t := range m.Spec.Transitions {
		if t.On != "" && !ids[t.On] {
			c.errorf(CodeUnknownEvent, t.From+"->"+t.To, "transition %s->%s fires on undeclared event %q", t.From, t.To, t.On)
		}
	}
	for _, b := range m.Spec.Bookkeeping {
		if !ids[b.On] {
			c.errorf(CodeUnknownEvent, b.Action, "bookkeeping %q fires on undeclared event %q", b.Action, b.On)
		}
	}
}

// predicate checks a predicate against the fact vocabulary.
func (c *compiler) predicate(subject string, p Predicate) {
	ft, ok := Facts[p.Fact]
	if !ok {
		c.errorf(CodeUnknownFact, subject, "fact %q is not in the vocabulary", p.Fact)
		return
	}
	valid := map[FactType][]Op{
		FactBool:   {OpTrue, OpFalse},
		FactScalar: {OpIs, OpNot},
		FactSet:    {OpHas, OpLacks},
	}[ft]
	for _, op := range valid {
		if op == p.Op {
			if ft != FactBool && p.Value == "" {
				c.errorf(CodeBadOp, subject, "fact %q %s needs a value", p.Fact, p.Op)
			}
			return
		}
	}
	c.errorf(CodeBadOp, subject, "fact %q is %s; operator %q does not apply", p.Fact, ft, p.Op)
}

// derivation checks the mode and compiles the rules.
func (c *compiler) derivation() {
	d := c.m.Spec.Derivation
	switch d.Mode {
	case DerivationBuiltin:
		if len(d.Rules) > 0 {
			c.warnf(CodeBadMode, "", "derivation is builtin; %d rules are ignored", len(d.Rules))
		}
	case DerivationRules:
		if len(d.Rules) == 0 {
			c.errorf(CodeNoRules, "", "derivation is rules but spec.derivation.rules is empty")
		}
		for i, r := range d.Rules {
			subject := r.ID
			if subject == "" {
				subject = fmt.Sprintf("rules[%d]", i)
			}
			if _, ok := c.p.States[r.State]; !ok {
				c.errorf(CodeUnknownState, subject, "rule %s puts items in unknown state %q", subject, r.State)
			}
			if len(r.When) == 0 {
				c.warnf(CodeNoRules, subject, "rule %s has no predicates and matches everything after it is reached", subject)
			}
			for _, p := range r.When {
				c.predicate(subject, p)
			}
		}
		c.p.Rules = append([]Rule(nil), d.Rules...)
	default:
		c.errorf(CodeBadMode, "", "spec.derivation.mode %q is not builtin or rules", d.Mode)
	}
}

// inferred warns about every element a human has not confirmed.
func (c *compiler) inferred() {
	for _, s := range c.m.Spec.States {
		if s.Lifecycle.Confidence == ConfidenceInferred {
			c.warnf(CodeInferred, s.ID, "state %q is inferred and unconfirmed", s.ID)
		}
	}
	for _, e := range c.m.Spec.Events {
		if e.Confidence == ConfidenceInferred {
			c.warnf(CodeInferred, e.ID, "event %q is inferred and unconfirmed", e.ID)
		}
	}
	for _, g := range c.m.Spec.Gates {
		if g.Confidence == ConfidenceInferred {
			c.warnf(CodeInferred, g.ID, "gate %q is inferred and unconfirmed", g.ID)
		}
	}
}

// StateForNumber returns the state carrying stage number n, if any.
func (p *Profile) StateForNumber(n int) (string, bool) {
	s, ok := p.StateOfNumber[n]
	return s, ok
}

// Column returns the column with id, or nil.
func (p *Profile) Column(id string) *Column {
	for i := range p.Columns {
		if p.Columns[i].ID == id {
			return &p.Columns[i]
		}
	}
	return nil
}

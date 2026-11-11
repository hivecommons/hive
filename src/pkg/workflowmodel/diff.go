package workflowmodel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ModelDiff is the structural difference between two models
// (hivecommons/hive#17620, RFC phase 4). It is computed from the spec
// only, never from metadata, and is deterministic: the same pair always
// produces the same diff, with every list sorted by key.
type ModelDiff struct {
	// Columns are keyed by column id.
	Columns ChangeSet `json:"columns"`
	// States are keyed by state id; regions are flattened like Compile.
	States ChangeSet `json:"states"`
	// Transitions are keyed "from>to" ("from>to@on" when a pair occurs
	// more than once).
	Transitions ChangeSet `json:"transitions"`
	// Labels are keyed by label name.
	Labels ChangeSet `json:"labels"`
	// Rules are keyed by rule id, or "rules[i]" for an unnamed rule.
	Rules ChangeSet `json:"rules"`
	// Meta lists the spec.meta fields that differ.
	Meta []FieldChange `json:"meta,omitempty"`
	// Summary is a one-line human description ("+2 states, −1
	// transition, 3 changed"; "no changes" when Empty).
	Summary string `json:"summary"`
	Empty   bool   `json:"empty"`
}

// ChangeSet is the diff of one keyed collection.
type ChangeSet struct {
	Added   []string     `json:"added,omitempty"`
	Removed []string     `json:"removed,omitempty"`
	Changed []ItemChange `json:"changed,omitempty"`
}

// Len is the number of items added, removed or changed.
func (c ChangeSet) Len() int { return len(c.Added) + len(c.Removed) + len(c.Changed) }

// ItemChange is one item present in both models whose fields differ.
type ItemChange struct {
	Key    string        `json:"key"`
	Fields []FieldChange `json:"fields"`
}

// FieldChange is one scalar field that differs. Lists are rendered as
// comma-joined strings so the diff stays flat and JSON-friendly.
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Diff compares a and b structurally. Either may be nil, which is treated
// as an empty model.
func Diff(a, b *Model) ModelDiff {
	if a == nil {
		a = &Model{}
	}
	if b == nil {
		b = &Model{}
	}
	d := ModelDiff{
		Columns:     diffItems(columnItems(a), columnItems(b)),
		States:      diffItems(stateItems(a), stateItems(b)),
		Transitions: diffItems(transitionItems(a), transitionItems(b)),
		Labels:      diffItems(labelItems(a), labelItems(b)),
		Rules:       diffItems(ruleItems(a), ruleItems(b)),
		Meta:        fieldChanges(metaFields(a), metaFields(b)),
	}
	d.Empty = d.Columns.Len()+d.States.Len()+d.Transitions.Len()+d.Labels.Len()+d.Rules.Len()+len(d.Meta) == 0
	d.Summary = d.summary()
	return d
}

func (d ModelDiff) summary() string {
	if d.Empty {
		return "no changes"
	}
	var parts []string
	changed := len(d.Meta)
	for _, c := range []struct {
		noun string
		set  ChangeSet
	}{
		{"column", d.Columns}, {"state", d.States}, {"transition", d.Transitions}, {"label", d.Labels}, {"rule", d.Rules},
	} {
		if n := len(c.set.Added); n > 0 {
			parts = append(parts, "+"+strconv.Itoa(n)+" "+plural(c.noun, n))
		}
		if n := len(c.set.Removed); n > 0 {
			parts = append(parts, "−"+strconv.Itoa(n)+" "+plural(c.noun, n))
		}
		changed += len(c.set.Changed)
	}
	if changed > 0 {
		parts = append(parts, strconv.Itoa(changed)+" changed")
	}
	return strings.Join(parts, ", ")
}

func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// item is one keyed element with its comparable fields in a fixed order.
type item struct {
	key    string
	fields []field
}

type field struct{ name, value string }

func diffItems(a, b []item) ChangeSet {
	var cs ChangeSet
	am := map[string]item{}
	for _, it := range a {
		am[it.key] = it
	}
	bm := map[string]item{}
	for _, it := range b {
		bm[it.key] = it
	}
	for k := range am {
		if _, ok := bm[k]; !ok {
			cs.Removed = append(cs.Removed, k)
		}
	}
	for k, bi := range bm {
		ai, ok := am[k]
		if !ok {
			cs.Added = append(cs.Added, k)
			continue
		}
		if fc := fieldChanges(ai.fields, bi.fields); len(fc) > 0 {
			cs.Changed = append(cs.Changed, ItemChange{Key: k, Fields: fc})
		}
	}
	sort.Strings(cs.Added)
	sort.Strings(cs.Removed)
	sort.Slice(cs.Changed, func(i, j int) bool { return cs.Changed[i].Key < cs.Changed[j].Key })
	return cs
}

// fieldChanges compares two field lists by name; a field present on one
// side only is reported against the empty string.
func fieldChanges(a, b []field) []FieldChange {
	av := map[string]string{}
	for _, f := range a {
		av[f.name] = f.value
	}
	var out []FieldChange
	seen := map[string]bool{}
	for _, f := range b {
		seen[f.name] = true
		if av[f.name] != f.value {
			out = append(out, FieldChange{Field: f.name, From: av[f.name], To: f.value})
		}
	}
	for _, f := range a {
		if !seen[f.name] && f.value != "" {
			out = append(out, FieldChange{Field: f.name, From: f.value, To: ""})
		}
	}
	return out
}

func join(ss []string) string { return strings.Join(ss, ",") }

func joinKinds(ks []ItemKind) string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return join(out)
}

func columnItems(m *Model) []item {
	var out []item
	for _, c := range m.Spec.Columns {
		out = append(out, item{key: c.ID, fields: []field{
			{"label", c.Label}, {"hint", c.Hint}, {"states", join(c.States)}, {"terminal", strconv.FormatBool(c.Terminal)},
		}})
	}
	return out
}

func stateItems(m *Model) []item {
	var out []item
	var walk func(states []State, parent string)
	walk = func(states []State, parent string) {
		for _, s := range states {
			out = append(out, item{key: s.ID, fields: []field{
				{"label", s.Label}, {"owner", string(s.Owner)}, {"type", string(s.Type)}, {"parent", parent},
				{"guardRef", s.GuardRef}, {"pipelines", join(s.Pipelines)}, {"origin", string(s.Origin)},
				{"column", s.Lifecycle.Column}, {"number", strconv.Itoa(s.Lifecycle.Number)},
				{"kinds", joinKinds(s.Lifecycle.Kinds)}, {"waiting", join(s.Lifecycle.Waiting)},
				{"replies", join(s.Lifecycle.Replies)}, {"confidence", string(s.Lifecycle.Confidence)},
			}})
			walk(s.Regions, s.ID)
		}
	}
	walk(m.Spec.States, "")
	return out
}

func transitionItems(m *Model) []item {
	pairs := map[string]int{}
	for _, t := range m.Spec.Transitions {
		pairs[t.From+">"+t.To]++
	}
	var out []item
	for _, t := range m.Spec.Transitions {
		key := t.From + ">" + t.To
		if pairs[key] > 1 {
			key += "@" + t.On
		}
		out = append(out, item{key: key, fields: []field{
			{"on", t.On}, {"guardRef", t.GuardRef}, {"gateRef", t.GateRef}, {"note", t.Note},
		}})
	}
	return out
}

func labelItems(m *Model) []item {
	var out []item
	for _, l := range m.Spec.Labels.Entries {
		out = append(out, item{key: l.Name, fields: []field{
			{"color", l.Color}, {"description", l.Description}, {"state", l.State},
		}})
	}
	return out
}

func ruleItems(m *Model) []item {
	var out []item
	for i, r := range m.Spec.Derivation.Rules {
		key := r.ID
		if key == "" {
			key = fmt.Sprintf("rules[%d]", i)
		}
		preds := make([]string, len(r.When))
		for j, p := range r.When {
			preds[j] = p.Fact + " " + string(p.Op) + " " + p.Value
		}
		out = append(out, item{key: key, fields: []field{
			{"kind", string(r.Kind)}, {"state", r.State}, {"when", strings.Join(preds, "; ")}, {"reason", r.Reason},
		}})
	}
	return out
}

func metaFields(m *Model) []field {
	mt := m.Spec.Meta
	return []field{
		{"version", mt.Version}, {"author", mt.Author}, {"description", mt.Description}, {"license", mt.License},
		{"source", string(mt.Source)}, {"provenance", string(mt.Provenance)}, {"mode", string(m.Spec.Derivation.Mode)},
	}
}

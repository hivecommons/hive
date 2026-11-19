package workflowmodel

import "encoding/json"

// AnnotationConfirmedBy records who confirmed an ingested model's inferred
// elements (Confirm). It lives in metadata.annotations so the spec hash
// stays a function of the spec alone.
const AnnotationConfirmedBy = "workflow.hivecommons.dev/confirmed-by"

// Confirm returns a deep copy of m in which every inferred element
// (states at any depth, events, gates) is marked confirmed, provenance is
// local and the version is cleared so the store assigns the next one. The
// source is kept: the model still says where it was ingested from. m is
// not modified.
func Confirm(m *Model, by string) *Model {
	if m == nil {
		return nil
	}
	out := Clone(m)
	var walk func(states []State)
	walk = func(states []State) {
		for i := range states {
			states[i].Lifecycle.Confidence = ConfidenceConfirmed
			walk(states[i].Regions)
		}
	}
	walk(out.Spec.States)
	for i := range out.Spec.Events {
		out.Spec.Events[i].Confidence = ConfidenceConfirmed
	}
	for i := range out.Spec.Gates {
		out.Spec.Gates[i].Confidence = ConfidenceConfirmed
	}
	out.Spec.Meta.Provenance = ProvenanceLocal
	out.Spec.Meta.Version = ""
	if by != "" {
		if out.Metadata.Annotations == nil {
			out.Metadata.Annotations = map[string]string{}
		}
		out.Metadata.Annotations[AnnotationConfirmedBy] = by
	}
	return out
}

// Clone returns a deep copy of m via its JSON form.
func Clone(m *Model) *Model {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	var out Model
	_ = json.Unmarshal(b, &out)
	return &out
}

// Inferred counts the elements a human has not confirmed yet.
func Inferred(m *Model) int {
	if m == nil {
		return 0
	}
	n := 0
	var walk func(states []State)
	walk = func(states []State) {
		for _, s := range states {
			if s.Lifecycle.Confidence == ConfidenceInferred {
				n++
			}
			walk(s.Regions)
		}
	}
	walk(m.Spec.States)
	for _, e := range m.Spec.Events {
		if e.Confidence == ConfidenceInferred {
			n++
		}
	}
	for _, g := range m.Spec.Gates {
		if g.Confidence == ConfidenceInferred {
			n++
		}
	}
	return n
}

// LabelDef is one label a Profile asks the engine to reconcile into a
// bound repository.
type LabelDef struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
}

// LabelDefs is the label vocabulary the profile reconciles: its label
// entries with defaultColor filled in where an entry names none and the
// state's label as description where an entry has none. Entries are
// returned in declaration order; a model with no labels block regenerates
// nothing and keeps using the built-in family by stage number.
func (p *Profile) LabelDefs(defaultColor string) []LabelDef {
	if p == nil {
		return nil
	}
	out := make([]LabelDef, 0, len(p.Labels.Entries))
	for _, e := range p.Labels.Entries {
		if e.Name == "" {
			continue
		}
		d := LabelDef{Name: e.Name, Color: e.Color, Description: e.Description, State: e.State}
		if d.Color == "" {
			d.Color = defaultColor
		}
		if d.Description == "" && e.State != "" {
			if s, ok := p.States[e.State]; ok && s != nil {
				d.Description = s.Label
			}
		}
		out = append(out, d)
	}
	return out
}

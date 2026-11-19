package workflowmodel

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDiffIdenticalIsEmpty(t *testing.T) {
	a := techdocs(t)
	d := Diff(a, Clone(a))
	if !d.Empty || d.Summary != "no changes" {
		t.Fatalf("diff of identical models: %+v", d)
	}
	if nd := Diff(nil, nil); !nd.Empty {
		t.Fatalf("nil diff not empty: %+v", nd)
	}
	if _, err := json.Marshal(d); err != nil {
		t.Fatal(err)
	}
}

func TestDiffStatesAddedRemovedChanged(t *testing.T) {
	a := techdocs(t)
	b := Clone(a)
	// change one, remove one, add one (with a region to exercise flattening)
	b.Spec.States[0].Label = "Requested (new)"
	b.Spec.States[0].Lifecycle.Number = 11
	b.Spec.States[0].Lifecycle.Replies = []string{"triage"}
	removed := b.Spec.States[len(b.Spec.States)-1].ID
	b.Spec.States = b.Spec.States[:len(b.Spec.States)-1]
	b.Spec.States = append(b.Spec.States, State{ID: "parallel", Owner: OwnerHuman, Type: StateParallel, Regions: []State{{ID: "inner", Owner: OwnerHuman, Lifecycle: StateLifecycle{Confidence: ConfidenceInferred}}}})

	d := Diff(a, b)
	if d.Empty {
		t.Fatal("expected changes")
	}
	if !reflect.DeepEqual(d.States.Added, []string{"inner", "parallel"}) {
		t.Fatalf("added %v", d.States.Added)
	}
	if !reflect.DeepEqual(d.States.Removed, []string{removed}) {
		t.Fatalf("removed %v", d.States.Removed)
	}
	if len(d.States.Changed) != 1 || d.States.Changed[0].Key != a.Spec.States[0].ID {
		t.Fatalf("changed %+v", d.States.Changed)
	}
	fields := map[string]FieldChange{}
	for _, f := range d.States.Changed[0].Fields {
		fields[f.Field] = f
	}
	if fields["label"].To != "Requested (new)" || fields["number"] != (FieldChange{"number", "10", "11"}) || fields["replies"] != (FieldChange{"replies", "", "triage"}) {
		t.Fatalf("fields %+v", fields)
	}
	if len(fields) != 3 {
		t.Fatalf("unexpected field changes: %+v", fields)
	}
	if d.Summary != "+2 states, −1 state, 1 changed" {
		t.Fatalf("summary %q", d.Summary)
	}
	// reverse direction swaps added/removed
	r := Diff(b, a)
	if !reflect.DeepEqual(r.States.Removed, []string{"inner", "parallel"}) || !reflect.DeepEqual(r.States.Added, []string{removed}) {
		t.Fatalf("reverse %+v", r.States)
	}
}

func TestDiffTransitionsLabelsColumnsRulesMeta(t *testing.T) {
	a := techdocs(t)
	b := Clone(a)
	b.Spec.Transitions[0].Note = "edited"
	b.Spec.Transitions[0].GuardRef = "g1"
	b.Spec.Transitions = b.Spec.Transitions[1:]
	b.Spec.Transitions = append(b.Spec.Transitions, Transition{From: "approved", To: "withdrawn", On: "closed"}, Transition{From: "approved", To: "withdrawn", On: "rejected"})
	b.Spec.Labels.Entries = append(b.Spec.Labels.Entries, LabelEntry{Name: "stage/010-requested", State: "requested"})
	b.Spec.Columns[0].Label = "Inbox"
	b.Spec.Columns = append(b.Spec.Columns, Column{ID: "extra", States: []string{"requested"}})
	b.Spec.Derivation.Rules = append(b.Spec.Derivation.Rules, Rule{State: "requested", When: []Predicate{{Fact: "open", Op: OpTrue}}}, Rule{ID: "named", State: "withdrawn"})
	b.Spec.Meta.Version = "2"
	b.Spec.Meta.Provenance = ProvenanceLocal

	d := Diff(a, b)
	removedKey := a.Spec.Transitions[0].From + ">" + a.Spec.Transitions[0].To
	if !reflect.DeepEqual(d.Transitions.Removed, []string{removedKey}) {
		t.Fatalf("transitions removed %v", d.Transitions.Removed)
	}
	if !reflect.DeepEqual(d.Transitions.Added, []string{"approved>withdrawn@closed", "approved>withdrawn@rejected"}) {
		t.Fatalf("transitions added %v", d.Transitions.Added)
	}
	if d.Transitions.Len() != 3 {
		t.Fatalf("transitions %+v", d.Transitions)
	}
	if !reflect.DeepEqual(d.Labels.Added, []string{"stage/010-requested"}) || d.Labels.Len() != 1 {
		t.Fatalf("labels %+v", d.Labels)
	}
	if !reflect.DeepEqual(d.Columns.Added, []string{"extra"}) || len(d.Columns.Changed) != 1 || d.Columns.Changed[0].Fields[0] != (FieldChange{"label", a.Spec.Columns[0].Label, "Inbox"}) {
		t.Fatalf("columns %+v", d.Columns)
	}
	wantRules := []string{"named", "rules[" + strconv.Itoa(len(a.Spec.Derivation.Rules)) + "]"}
	if !reflect.DeepEqual(d.Rules.Added, wantRules) {
		t.Fatalf("rules %v want %v", d.Rules.Added, wantRules)
	}
	meta := map[string]FieldChange{}
	for _, f := range d.Meta {
		meta[f.Field] = f
	}
	if meta["version"] != (FieldChange{"version", "1", "2"}) || meta["provenance"].To != string(ProvenanceLocal) || len(meta) != 2 {
		t.Fatalf("meta %+v", d.Meta)
	}
	for _, want := range []string{"+1 column", "−1 transition", "+2 transitions", "+1 label", "+2 rules", "changed"} {
		if !strings.Contains(d.Summary, want) {
			t.Fatalf("summary %q lacks %q", d.Summary, want)
		}
	}
	// a transition whose fields change is reported as changed
	c := Clone(a)
	c.Spec.Transitions[0].Note = "why"
	cd := Diff(a, c)
	if len(cd.Transitions.Changed) != 1 || cd.Transitions.Changed[0].Fields[0] != (FieldChange{"note", a.Spec.Transitions[0].Note, "why"}) {
		t.Fatalf("transition change %+v", cd.Transitions)
	}
	if cd.Summary != "1 changed" {
		t.Fatalf("summary %q", cd.Summary)
	}
}

func TestConfirmClearsInferredAndLocalises(t *testing.T) {
	a := techdocs(t)
	a.Spec.Meta.Provenance = ProvenanceIngestedUnreviewed
	a.Spec.Meta.Source = SourceGitHubActions
	a.Spec.States[0].Lifecycle.Confidence = ConfidenceInferred
	a.Spec.States = append(a.Spec.States, State{ID: "par", Owner: OwnerHuman, Type: StateParallel, Regions: []State{{ID: "in", Owner: OwnerHuman, Lifecycle: StateLifecycle{Confidence: ConfidenceInferred}}}})
	a.Spec.Events = append(a.Spec.Events, Event{ID: "ev", Fact: Predicate{Fact: "open", Op: OpTrue}, Confidence: ConfidenceInferred})
	a.Spec.Gates = append(a.Spec.Gates, Gate{ID: "gt", ReleasedBy: "maintainer", Confidence: ConfidenceInferred})
	if Inferred(a) != 4 {
		t.Fatalf("inferred %d", Inferred(a))
	}
	before := a.Hash()

	c := Confirm(a, "andy")
	if Inferred(c) != 0 {
		t.Fatalf("confirmed model still has %d inferred", Inferred(c))
	}
	if c.Spec.Meta.Provenance != ProvenanceLocal || c.Spec.Meta.Version != "" || c.Spec.Meta.Source != SourceGitHubActions {
		t.Fatalf("meta %+v", c.Spec.Meta)
	}
	if c.Metadata.Annotations[AnnotationConfirmedBy] != "andy" {
		t.Fatalf("annotations %v", c.Metadata.Annotations)
	}
	if c.Metadata.Name != a.Metadata.Name || len(c.Spec.States) != len(a.Spec.States) {
		t.Fatal("confirm lost structure")
	}
	// original untouched
	if a.Hash() != before || Inferred(a) != 4 || a.Spec.Meta.Provenance != ProvenanceIngestedUnreviewed || a.Metadata.Annotations != nil {
		t.Fatal("Confirm mutated its input")
	}
	// no annotation without a user; nil in → nil out
	if Confirm(a, "").Metadata.Annotations != nil || Confirm(nil, "x") != nil || Clone(nil) != nil || Inferred(nil) != 0 {
		t.Fatal("edge cases")
	}
}

func TestProfileLabelDefs(t *testing.T) {
	m := techdocs(t)
	m.Spec.Labels = LabelSet{Prefix: "stage", Entries: []LabelEntry{
		{Name: "stage/010-requested", State: "requested"},
		{Name: "stage/020-analysing", Color: "123456", Description: "explicit", State: "analysing"},
		{Name: "", State: "ghost"},
		{Name: "orphan", State: "nope"},
	}}
	p, _, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	got := p.LabelDefs("abcdef")
	want := []LabelDef{
		{Name: "stage/010-requested", Color: "abcdef", Description: m.Spec.States[0].Label, State: "requested"},
		{Name: "stage/020-analysing", Color: "123456", Description: "explicit", State: "analysing"},
		{Name: "orphan", Color: "abcdef", State: "nope"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if (*Profile)(nil).LabelDefs("x") != nil {
		t.Fatal("nil profile")
	}
	if n := len(techdocsProfile(t).LabelDefs("x")); n != 0 {
		t.Fatalf("model without labels block yields %d defs", n)
	}
}

func techdocsProfile(t *testing.T) *Profile {
	t.Helper()
	p, _, err := Compile(techdocs(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

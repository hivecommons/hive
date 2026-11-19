package workflowmodel

import (
	"reflect"
	"testing"
)

// The convention: a grant's first state is its entry state; a profile that
// produces neither a pull request nor a push and has no gated exit reply is
// still dispatched on entry.
func TestDispatchConvention(t *testing.T) {
	p, _, err := Compile(techdocs(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Agents) != 2 {
		t.Fatalf("agents %+v", p.Agents)
	}
	a, ok := p.DispatchFor("analysing")
	if !ok || a.Profile != "techdocs-analyst" || a.Reply != "" || a.Explicit || a.Deliverable != nil {
		t.Fatalf("analysing dispatch = %+v %v", a, ok)
	}
	if !a.AppliesTo(KindIssue) || a.AppliesTo(KindPR) {
		t.Fatalf("analysing kinds %v", a.Kinds)
	}
	w, ok := p.DispatchFor("drafting")
	if !ok || w.Profile != "techdocs-writer" || w.Reply != "" {
		t.Fatalf("drafting dispatch = %+v %v", w, ok)
	}
	if _, ok := p.DispatchFor("analysis-review"); ok {
		t.Fatal("a human state must not dispatch")
	}
	if !p.Dispatches() {
		t.Fatal("Dispatches() false")
	}
}

// A reviewing profile (no pull request) is dispatched on the reply that
// releases its entry state's gate into a non-terminal state.
func TestDispatchReviewerWaitsForGateReply(t *testing.T) {
	m := techdocs(t)
	m.Spec.Agents = []AgentGrant{{Profile: "checker", States: []string{"draft-review"}, Produces: []string{"comment"}}}
	p, diags, err := Compile(m)
	if err != nil {
		t.Fatalf("%v %+v", err, diags)
	}
	d, ok := p.DispatchFor("draft-review")
	if !ok || d.Reply != "approve" {
		t.Fatalf("draft-review dispatch = %+v %v", d, ok)
	}
}

func TestDispatchExplicit(t *testing.T) {
	m := techdocs(t)
	m.Spec.Agents[1].Dispatch = &AgentDispatch{States: []string{"drafting"}, Reply: "approve", Instructions: "Open a draft PR."}
	p, diags, err := Compile(m)
	if err != nil {
		t.Fatalf("%v %+v", err, diags)
	}
	d, ok := p.DispatchFor("drafting")
	if !ok || !d.Explicit || d.Reply != "approve" || len(d.Instructions) == 0 || d.Instructions[0] != "Open a draft PR." {
		t.Fatalf("drafting dispatch = %+v %v", d, ok)
	}
	found := false
	for _, dg := range diags {
		if dg.Code == CodeDispatchReply && dg.Subject == "drafting" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want %s warning for an undeclared reply, got %+v", CodeDispatchReply, diags)
	}

	// An empty explicit block turns dispatch off for the grant.
	m = techdocs(t)
	m.Spec.Agents[1].Dispatch = &AgentDispatch{}
	p, _, err = Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.DispatchFor("drafting"); ok {
		t.Fatal("empty dispatch block must not dispatch")
	}
}

func TestDispatchRefusals(t *testing.T) {
	m := techdocs(t)
	m.Spec.Agents[0].Dispatch = &AgentDispatch{States: []string{"drafting"}}
	if _, diags, err := Compile(m); err == nil || !hasCode(diags, CodeUnknownState) {
		t.Fatalf("dispatch outside the grant: err=%v diags=%+v", err, diags)
	}
	m = techdocs(t)
	m.Spec.Agents[0].States = []string{"analysing", "drafting"}
	m.Spec.Agents[0].Dispatch = &AgentDispatch{States: []string{"drafting"}}
	if _, diags, err := Compile(m); err == nil || !hasCode(diags, CodeDuplicateDispatch) {
		t.Fatalf("two profiles in one state: err=%v diags=%+v", err, diags)
	}
}

func TestDispatchDeliverable(t *testing.T) {
	m := techdocs(t)
	m.Spec.Agents = []AgentGrant{{Profile: "drafter", States: []string{"analysing", "drafting"}, Produces: []string{"pull-request"}}}
	m.Spec.Derivation.Rules = append([]Rule{{ID: "d", Kind: KindPR, State: "drafting", When: []Predicate{
		{Fact: "draft", Op: OpTrue}, {Fact: "labels", Op: OpHas, Value: "docs"}, {Fact: "labels", Op: OpHas, Value: "phase/*"},
	}}}, m.Spec.Derivation.Rules...)
	p, diags, err := Compile(m)
	if err != nil {
		t.Fatalf("%v %+v", err, diags)
	}
	d, ok := p.DispatchFor("analysing")
	if !ok || d.Deliverable == nil {
		t.Fatalf("dispatch %+v %v", d, ok)
	}
	want := &Deliverable{State: "drafting", Draft: true, Labels: []string{"docs"}}
	if !reflect.DeepEqual(d.Deliverable, want) {
		t.Fatalf("deliverable %+v want %+v", d.Deliverable, want)
	}
}

func TestDispatchNeverForHiveDefault(t *testing.T) {
	m := techdocs(t)
	m.Metadata.Name = hiveDefaultModelName
	p, _, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	if p.Dispatches() {
		t.Fatalf("hive-default dispatches %+v", p.Dispatch)
	}
	var nilProfile *Profile
	if _, ok := nilProfile.DispatchFor("x"); ok || nilProfile.Dispatches() {
		t.Fatal("nil profile dispatches")
	}
}

func hasCode(diags []Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

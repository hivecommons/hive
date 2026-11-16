package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func TestModelInitialStateAndUnnumberedStates(t *testing.T) {
	p := &workflowmodel.Profile{
		Name: "custom", Mode: workflowmodel.DerivationRules, Initial: "intake",
		States: map[string]*workflowmodel.State{
			"intake": {Lifecycle: workflowmodel.StateLifecycle{Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
			"triage": {Lifecycle: workflowmodel.StateLifecycle{Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
		},
		Labels: workflowmodel.LabelSet{Entries: []workflowmodel.LabelEntry{{Name: "model/intake", State: "intake"}, {Name: "model/triage", State: "triage"}}},
		Rules: []workflowmodel.Rule{
			{Kind: workflowmodel.KindIssue, State: "triage", When: []workflowmodel.Predicate{{Fact: "labels", Op: workflowmodel.OpHas, Value: "needs-review"}}},
			{Kind: workflowmodel.KindIssue, State: "intake", When: []workflowmodel.Predicate{{Fact: "labels", Op: workflowmodel.OpHas, Value: "new-request"}}},
		},
	}
	d := DeriveFromModel(p, stagelabels.IssueItem)
	f := Facts{Ref: Ref{Kind: stagelabels.IssueItem}, Open: true, Issue: &IssueFacts{}, Labels: []string{"new-request"}}
	got := d.Derive(f)
	if got.Abstain || got.ModelState != "intake" || got.ModelProfile != p {
		t.Fatalf("initial: %+v", got)
	}
	f.Labels = []string{"needs-review"}
	got = d.Derive(f)
	if got.Abstain || got.ModelState != "triage" || got.Terminal {
		t.Fatalf("triage: %+v", got)
	}
	f.Labels = []string{"model/triage"}
	if got = d.Derive(f); !got.Abstain {
		t.Fatalf("existing state must not be reset: %+v", got)
	}
	f.Ref.Kind = stagelabels.PRItem
	if got = DeriveFromModel(p, stagelabels.PRItem).Derive(f); !got.Abstain {
		t.Fatalf("issue state applied to PR: %+v", got)
	}
}

func TestIngestedIntakeSelection(t *testing.T) {
	p := &workflowmodel.Profile{
		Initial: "open",
		States: map[string]*workflowmodel.State{
			"request": {Lifecycle: workflowmodel.StateLifecycle{Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
			"triage":  {Lifecycle: workflowmodel.StateLifecycle{Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
		},
		ColumnOf: map[string]string{"request": "intake", "triage": "triage"},
		Events: []workflowmodel.Event{
			{ID: "template", Fact: workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: "kind/bug"}},
			{ID: "triage-requested", Fact: workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: "needs-triage"}},
			{ID: "triage-accepted", Fact: workflowmodel.Predicate{Fact: "labels", Op: workflowmodel.OpHas, Value: "triage/accepted"}},
		},
		Transitions: []workflowmodel.Transition{{From: "open", To: "request", On: "template"}, {From: "open", To: "triage", On: "triage-requested"}},
	}
	f := Facts{Ref: Ref{Kind: stagelabels.IssueItem}, Open: true, Labels: []string{"kind/bug"}}
	if got := intakeState(p, f); got != "request" {
		t.Fatalf("template: %q", got)
	}
	f.Labels = append(f.Labels, "needs-triage")
	if got := intakeState(p, f); got != "triage" {
		t.Fatalf("triage: %q", got)
	}
	f.Labels = append(f.Labels, "triage/accepted")
	if got := intakeState(p, f); got != "" {
		t.Fatalf("accepted: %q", got)
	}
}

func TestModelDoesNotManufactureGateApproval(t *testing.T) {
	p := &workflowmodel.Profile{
		States:        map[string]*workflowmodel.State{"actionable": {}},
		Labels:        workflowmodel.LabelSet{Entries: []workflowmodel.LabelEntry{{Name: "triage/accepted", State: "actionable"}}},
		GuardedLabels: []string{"triage/accepted"},
	}
	var want stagelabels.Desired
	modelDesired(&want, p, "actionable")
	add, _ := stagelabels.Plan(nil, want)
	for _, label := range add {
		if label == "triage/accepted" {
			t.Fatal("model must not release a human triage gate")
		}
	}
}

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

// hivecommons/hive#19611: a numbered state's own stage label is the one the
// Decision asks the writer for; the stage (and so the store record) is
// still the built-in stage at that number.
func TestModelStateStageLabel(t *testing.T) {
	p := &workflowmodel.Profile{
		Name: "custom", Mode: workflowmodel.DerivationRules,
		States: map[string]*workflowmodel.State{
			"accepted": {Lifecycle: workflowmodel.StateLifecycle{Number: 20, StageLabel: "stage/020-accepted", Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
			"queued":   {Lifecycle: workflowmodel.StateLifecycle{Number: 10, Kinds: []workflowmodel.ItemKind{workflowmodel.KindIssue}}},
		},
		Rules: []workflowmodel.Rule{
			{Kind: workflowmodel.KindIssue, State: "accepted", When: []workflowmodel.Predicate{{Fact: "labels", Op: workflowmodel.OpHas, Value: "accepted"}}},
			{Kind: workflowmodel.KindIssue, State: "queued", When: []workflowmodel.Predicate{{Fact: "labels", Op: workflowmodel.OpHas, Value: "queued"}}},
		},
	}
	d := DeriveFromModel(p, stagelabels.IssueItem)
	f := Facts{Ref: Ref{Kind: stagelabels.IssueItem}, Open: true, Issue: &IssueFacts{}, Labels: []string{"accepted", stagelabels.Claimed.Label()}}
	got := d.Derive(f)
	if got.Abstain || got.Stage != stagelabels.Claimed || got.StageLabel != "stage/020-accepted" || got.Label() != "stage/020-accepted" {
		t.Fatalf("accepted: %+v label %q", got, got.Label())
	}
	add, remove := stagelabels.Plan(f.Labels, got.Desired())
	if a := stagelabels.StageLabels(add); len(a) != 1 || a[0] != "stage/020-accepted" || len(remove) != 1 || remove[0] != stagelabels.Claimed.Label() {
		t.Fatalf("plan add=%v remove=%v", add, remove)
	}
	f.Labels = []string{"queued"}
	if got = d.Derive(f); got.Stage != stagelabels.Actionable || got.StageLabel != "" || got.Label() != stagelabels.Actionable.Label() {
		t.Fatalf("queued keeps the built-in label: %+v", got)
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

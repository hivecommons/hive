package stageengine

import (
	"context"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func modelLabelProfile() *workflowmodel.Profile {
	return &workflowmodel.Profile{
		Name: "custom", Hash: "custom-v1", Mode: workflowmodel.DerivationRules, Initial: "initial",
		States: map[string]*workflowmodel.State{
			"initial": {ID: "initial"},
			"working": {ID: "working"},
			"done":    {ID: "done", Type: workflowmodel.StateFinal},
		},
		Terminal:    map[string]bool{"done": true},
		InputLabels: []string{"trigger/*", "kind/bug", "needs-triage"},
		Labels: workflowmodel.LabelSet{Entries: []workflowmodel.LabelEntry{
			{Name: "workflow/new", State: "initial"},
			{Name: "workflow/working", State: "working"},
			{Name: "workflow/done", State: "done"},
			{Name: "kind/bug", State: "initial"},
			{Name: "trigger/advance", State: "working"},
			{Name: "topic/not-a-state"},
		}},
		Rules: []workflowmodel.Rule{
			{State: "done", When: []workflowmodel.Predicate{{Fact: "open", Op: workflowmodel.OpFalse}}},
			{State: "working", When: []workflowmodel.Predicate{{Fact: "labels", Op: workflowmodel.OpHas, Value: "trigger/advance"}}},
			{State: "initial", When: []workflowmodel.Predicate{{Fact: "open", Op: workflowmodel.OpTrue}}},
		},
	}
}

func TestModelLabelsReconcileIssueAndPR(t *testing.T) {
	for _, kind := range []stagelabels.Item{stagelabels.IssueItem, stagelabels.PRItem} {
		t.Run(kind.String(), func(t *testing.T) {
			ctx := context.Background()
			p := modelLabelProfile()
			w := newFakeWriter()
			r := New(Options{Mode: ModeEnforce, Writer: w, Store: stagelabels.NewStore("", nil), WorkflowFor: func(string, string) *workflowmodel.Profile { return p }})
			f := Facts{Ref: Ref{Owner: "acme", Repo: "widget", Number: 1, Kind: kind}, Open: true}
			check := func(want, absent []string) {
				t.Helper()
				labels, err := w.Labels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
				if err != nil {
					t.Fatal(err)
				}
				for _, label := range want {
					if !slices.Contains(labels, label) {
						t.Errorf("labels %v missing %q", labels, label)
					}
				}
				for _, label := range absent {
					if slices.Contains(labels, label) {
						t.Errorf("labels %v unexpectedly contain %q", labels, label)
					}
				}
			}
			if _, err := r.Reconcile(ctx, f); err != nil {
				t.Fatal(err)
			}
			check([]string{"workflow/new", "kind/bug"}, []string{"trigger/advance"})

			// The externally supplied signal selects the next state. Existing
			// input labels remain, but the old model-owned label is removed.
			if err := w.AddLabels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number, []string{"trigger/advance"}); err != nil {
				t.Fatal(err)
			}
			f.Labels, _ = w.Labels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
			if _, err := r.Reconcile(ctx, f); err != nil {
				t.Fatal(err)
			}
			check([]string{"workflow/working", "kind/bug", "trigger/advance"}, []string{"workflow/new"})
			f.Labels, _ = w.Labels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
			before := w.Writes
			if _, err := r.Reconcile(ctx, f); err != nil {
				t.Fatal(err)
			}
			if w.Writes != before {
				t.Fatal("unchanged model state wrote labels again")
			}

			f.Open = false
			if _, err := r.Reconcile(ctx, f); err != nil {
				t.Fatal(err)
			}
			check([]string{"workflow/done", "kind/bug", "trigger/advance"}, []string{"workflow/working", "workflow/new"})
		})
	}
}

func TestUnnumberedModelStateLeavesBuiltInPipeline(t *testing.T) {
	ctx := context.Background()
	p := modelLabelProfile()
	w := newFakeWriter()
	store := stagelabels.NewStore("", nil)
	r := New(Options{Mode: ModeEnforce, Writer: w, Store: store, Workflow: p})
	f := Facts{
		Ref:    Ref{Owner: "acme", Repo: "widget", Number: 1, Kind: stagelabels.IssueItem},
		Open:   true,
		Labels: []string{"stage/010-actionable", "trigger/advance"},
	}
	if err := w.AddLabels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number, f.Labels); err != nil {
		t.Fatal(err)
	}
	store.Observe(f.Ref.Key(), stagelabels.Actionable, "previous stage")
	dec := DeriveFromModel(p, f.Ref.Kind).Derive(f)
	if dec.Abstain || dec.Terminal || dec.Stage != stagelabels.None || dec.ModelState != "working" {
		t.Fatalf("decision = %+v", dec)
	}
	tr, err := r.Reconcile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if tr.From != stagelabels.Actionable || tr.To != stagelabels.None || tr.Anomaly != "" {
		t.Fatalf("transition = %+v", tr)
	}
	labels, err := w.Labels(ctx, f.Ref.Owner, f.Ref.Repo, f.Ref.Number)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(labels, "stage/010-actionable") || !slices.Contains(labels, "workflow/working") {
		t.Fatalf("labels = %v", labels)
	}
	if record, ok := store.Get(f.Ref.Key()); ok {
		t.Fatalf("built-in stage record retained: %+v", record)
	}
}

func TestModelLabelPlannerPreservesInputsAndHonorsControls(t *testing.T) {
	p := modelLabelProfile()
	d := Decision{ModelProfile: p, ModelState: "working", Clear: []string{"needs-triage"}}
	want := d.Desired()
	add, remove := stagelabels.Plan([]string{"WORKFLOW/NEW", "kind/bug", "Trigger/advance", "needs-triage", "topic/not-a-state", "foreign"}, want)
	if !slices.Equal(add, []string{"workflow/working"}) || !slices.Equal(remove, []string{"WORKFLOW/NEW"}) {
		t.Fatalf("add=%v remove=%v", add, remove)
	}
	for _, d := range []Decision{{}, {ModelProfile: p}, {ModelProfile: p, ModelState: "missing"}} {
		want := d.Desired()
		if len(want.ModelLabels)+len(want.ModelManaged)+len(want.ModelPreserve) != 0 {
			t.Fatalf("unbound decision changed model labels: %+v", want)
		}
	}
}

func TestModelLabelsShadowDoesNotWrite(t *testing.T) {
	w := newFakeWriter()
	p := modelLabelProfile()
	r := New(Options{Mode: ModeShadow, Writer: w, Store: stagelabels.NewStore("", nil), Workflow: p})
	f := Facts{Ref: Ref{Owner: "acme", Repo: "widget", Number: 1, Kind: stagelabels.IssueItem}, Open: true}
	if _, err := r.Reconcile(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if w.Writes != 0 {
		t.Fatalf("shadow wrote %d times", w.Writes)
	}
}

package stageengine_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// TestDeriveFromModelHiveDefaultParity is the RFC's P1 bar
// (hivecommons/hive#17620): under the generated hive-default model every
// Decision is identical to the built-in derivers', across the whole PR
// golden corpus and the issue stage sweep.
func TestDeriveFromModelHiveDefaultParity(t *testing.T) {
	p := hivedefault.Profile()
	derivers := map[stagelabels.Item][2]stageengine.Deriver{
		stagelabels.IssueItem: {stageengine.IssueDeriver(), stageengine.DeriveFromModel(p, stagelabels.IssueItem)},
		stagelabels.PRItem:    {stageengine.PRDeriver(), stageengine.DeriveFromModel(p, stagelabels.PRItem)},
	}
	facts := stageengine.ParityFacts()
	if len(facts) < 100 {
		t.Fatalf("parity corpus has only %d facts", len(facts))
	}
	stages := map[stagelabels.Stage]bool{}
	for i, f := range facts {
		d := derivers[f.Ref.Kind]
		want, got := d[0].Derive(f), d[1].Derive(f)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("facts[%d] %s: built-in %+v, hive-default %+v", i, f.Ref.Key(), want, got)
		}
		if !want.Abstain && !want.Terminal {
			stages[want.Stage] = true
		}
	}
	if len(stages) < 15 {
		t.Errorf("corpus reached only %d stages: %v", len(stages), stages)
	}
}

// A nil profile is the built-in deriver, and a model that is missing a
// stage the built-in deriver produced abstains "not covered" rather than
// writing a label the model does not know.
func TestDeriveFromModelCoverage(t *testing.T) {
	f := stageengine.ParityFacts()[0]
	if f.Ref.Kind != stagelabels.PRItem {
		t.Fatalf("expected a PR fixture first, got %s", f.Ref.Kind)
	}
	nilD := stageengine.DeriveFromModel(nil, stagelabels.PRItem)
	if got, want := nilD.Derive(f), stageengine.PRDeriver().Derive(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil profile: %+v != %+v", got, want)
	}
	if stageengine.DeriveFromModel(nil, stagelabels.IssueItem).Derive(f).Abstain != true {
		t.Fatal("kind mismatch must abstain")
	}

	base := stageengine.PRDeriver().Derive(f)
	if base.Abstain || base.Terminal {
		t.Skipf("first fixture is %+v; coverage case needs a staged PR", base)
	}
	m := hivedefault.Generate()
	drop := hivedefault.StateID(base.Stage)
	var kept []workflowmodel.State
	for _, s := range m.Spec.States {
		if s.ID != drop {
			kept = append(kept, s)
		}
	}
	m.Spec.States = kept
	var tr []workflowmodel.Transition
	for _, x := range m.Spec.Transitions {
		if x.From != drop && x.To != drop {
			tr = append(tr, x)
		}
	}
	m.Spec.Transitions = tr
	for i := range m.Spec.Columns {
		var st []string
		for _, s := range m.Spec.Columns[i].States {
			if s != drop {
				st = append(st, s)
			}
		}
		m.Spec.Columns[i].States = st
	}
	for i := range m.Spec.Agents {
		var st []string
		for _, s := range m.Spec.Agents[i].States {
			if s != drop {
				st = append(st, s)
			}
		}
		m.Spec.Agents[i].States = st
	}
	p, _, err := workflowmodel.Compile(m)
	if err != nil {
		t.Fatalf("trimmed model: %v", err)
	}
	got := stageengine.DeriveFromModel(p, stagelabels.PRItem).Derive(f)
	if !got.Abstain || !strings.HasPrefix(got.Reason, stageengine.ReasonNotCovered) {
		t.Fatalf("missing state %s: got %+v", drop, got)
	}
}

// DeriveForRepo routes bound repositories through their model and leaves
// unbound repositories on the built-in deriver, decision for decision.
func TestDeriveForRepo(t *testing.T) {
	p := hivedefault.Profile()
	calls := 0
	lookup := func(owner, repo string) *workflowmodel.Profile {
		calls++
		if repo == "bound" {
			return p
		}
		return nil
	}
	if stageengine.DeriveForRepo(nil, stageengine.PRDeriver()) == nil || stageengine.DeriveForRepo(lookup, nil) != nil {
		t.Fatal("nil handling")
	}
	derivers := map[stagelabels.Item][2]stageengine.Deriver{
		stagelabels.IssueItem: {stageengine.IssueDeriver(), stageengine.DeriveForRepo(lookup, stageengine.IssueDeriver())},
		stagelabels.PRItem:    {stageengine.PRDeriver(), stageengine.DeriveForRepo(lookup, stageengine.PRDeriver())},
	}
	if derivers[stagelabels.PRItem][1].Kind() != stagelabels.PRItem {
		t.Fatal("kind")
	}
	for i, f := range stageengine.ParityFacts() {
		for _, repo := range []string{"bound", "free"} {
			f.Ref.Repo = repo
			d := derivers[f.Ref.Kind]
			if want, got := d[0].Derive(f), d[1].Derive(f); !reflect.DeepEqual(want, got) {
				t.Errorf("facts[%d] %s/%s: built-in %+v, routed %+v", i, repo, f.Ref.Key(), want, got)
			}
		}
	}
	if calls == 0 {
		t.Fatal("lookup never consulted")
	}
	// the engine composes it when Options.WorkflowFor is set
	r := stageengine.New(stageengine.Options{WorkflowFor: lookup, Store: stagelabels.NewStore(t.TempDir()+"/stages.json", nil)})
	if r == nil {
		t.Fatal("engine")
	}
}

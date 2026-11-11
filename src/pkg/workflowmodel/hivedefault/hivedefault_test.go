package hivedefault

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func TestGenerateCompiles(t *testing.T) {
	m := Generate()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	p, diags, err := workflowmodel.Compile(m)
	if err != nil {
		t.Fatalf("%v\n%+v", err, diags)
	}
	if len(diags) != 0 {
		t.Errorf("hive-default must compile clean, got %+v", diags)
	}
	if p.Name != Name || p.Version != Version || p.Mode != workflowmodel.DerivationBuiltin {
		t.Fatalf("profile %s@%s mode %s", p.Name, p.Version, p.Mode)
	}
	if Profile().Hash != p.Hash {
		t.Fatal("Profile() is not Generate() compiled")
	}
}

func TestEveryStageIsAState(t *testing.T) {
	p := Profile()
	ids := map[string]bool{}
	for _, d := range stagelabels.Table {
		id := StateID(d.Stage)
		if ids[id] {
			t.Errorf("StateID(%s) = %q collides", d.Stage, id)
		}
		ids[id] = true
		s, ok := p.States[id]
		if !ok {
			t.Errorf("stage %s has no state %q", d.Stage, id)
			continue
		}
		if s.Lifecycle.Number != d.Number {
			t.Errorf("state %q number %d, stage %d", id, s.Lifecycle.Number, d.Number)
		}
		if got, ok := p.StateForNumber(d.Number); !ok || got != id {
			t.Errorf("StateForNumber(%d) = %q %v, want %q", d.Number, got, ok, id)
		}
		if p.ColumnOf[id] == "" || p.ColumnOf[id] == ColumnOpen || p.ColumnOf[id] == ColumnMerged {
			t.Errorf("state %q sits in column %q", id, p.ColumnOf[id])
		}
		wantKind := workflowmodel.KindIssue
		if d.PR {
			wantKind = workflowmodel.KindPR
		}
		if len(s.Lifecycle.Kinds) != 1 || s.Lifecycle.Kinds[0] != wantKind {
			t.Errorf("state %q kinds %v, want [%s]", id, s.Lifecycle.Kinds, wantKind)
		}
	}
	if StateID(stagelabels.IssueNeedsDecision) == StateID(stagelabels.NeedsDecision) {
		t.Error("045 and 085 must have distinct state ids")
	}
}

func TestColumnsMatchTheMonitor(t *testing.T) {
	p := Profile()
	want := []string{ColumnOpen, ColumnActionable, ColumnClaimed, ColumnPRInProgress, ColumnCIReview, ColumnMergeQueue, ColumnMerged}
	if len(p.Columns) != len(want) {
		t.Fatalf("columns %+v", p.Columns)
	}
	for i, c := range p.Columns {
		if c.ID != want[i] {
			t.Errorf("column %d = %q, want %q", i, c.ID, want[i])
		}
		if c.Terminal != (c.ID == ColumnMerged) {
			t.Errorf("column %q terminal %v", c.ID, c.Terminal)
		}
	}
	// tracker-row-anchor.js: 15/17/18/45 light Actionable, 40 lights PR in
	// progress, 6x/7x light CI / Review, 85+ light Merge queue.
	for n, col := range map[int]string{10: ColumnActionable, 15: ColumnActionable, 45: ColumnActionable, 20: ColumnClaimed, 30: ColumnPRInProgress, 40: ColumnPRInProgress, 50: ColumnCIReview, 60: ColumnCIReview, 74: ColumnCIReview, 80: ColumnMergeQueue, 85: ColumnMergeQueue, 100: ColumnMergeQueue} {
		id, _ := p.StateForNumber(n)
		if p.ColumnOf[id] != col {
			t.Errorf("stage %d (%s) in %q, want %q", n, id, p.ColumnOf[id], col)
		}
	}
	if !p.Terminal[StateMerged] || !p.Terminal[StateClosed] || p.Terminal[StateOpen] {
		t.Errorf("terminal %v", p.Terminal)
	}
}

// The transition graph is the stage engine's expected-edge table plus
// intake and closure: no edge the engine would log as an anomaly.
func TestTransitionsMirrorTheEngine(t *testing.T) {
	m := Generate()
	have := map[[2]string]bool{}
	for _, tr := range m.Spec.Transitions {
		have[[2]string{tr.From, tr.To}] = true
	}
	for _, e := range stageengine.DiagramEdges() {
		k := [2]string{StateID(e.From), StateID(e.To)}
		if !have[k] {
			t.Errorf("engine edge %s -> %s missing from the model", e.From, e.To)
		}
	}
	for _, d := range stagelabels.Table {
		id := StateID(d.Stage)
		if !have[[2]string{id, StateClosed}] {
			t.Errorf("%s has no closure edge", id)
		}
	}
	if !have[[2]string{StateOpen, StateID(stagelabels.Actionable)}] || !have[[2]string{StateOpen, StateID(stagelabels.ReviewPending)}] {
		t.Error("intake edges missing")
	}
}

func TestModelRoundTripsJSON(t *testing.T) {
	m := Generate()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back workflowmodel.Model
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	p1, _, err := workflowmodel.Compile(&back)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Hash != Profile().Hash {
		t.Fatal("hash changed across a JSON round trip")
	}
}

func TestOwnerAndStateIDHelpers(t *testing.T) {
	if StateID(stagelabels.None) != "" {
		t.Error("None is not a state")
	}
	for _, d := range stagelabels.Table {
		if owner(d) == "" {
			t.Errorf("stage %s has no owner", d.Stage)
		}
		if label(d) == "" {
			t.Errorf("stage %s has no label", d.Stage)
		}
	}
}

package workflowmodel

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadModel(t *testing.T, path string) *Model {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func techdocs(t *testing.T) *Model { return loadModel(t, "testdata/models/techdocs.json") }

func TestCompileTechDocs(t *testing.T) {
	p, diags, err := Compile(techdocs(t))
	if err != nil {
		t.Fatalf("%v\n%+v", err, diags)
	}
	if p.Mode != DerivationRules || len(p.Rules) != 9 {
		t.Fatalf("mode %s rules %d", p.Mode, len(p.Rules))
	}
	if p.ColumnOf["draft-review"] != "writing" || !p.Terminal["published"] || p.Terminal["approved"] {
		t.Fatalf("columns %v terminal %v", p.ColumnOf, p.Terminal)
	}
	if id, ok := p.StateForNumber(45); !ok || id != "analysis-review" {
		t.Fatalf("StateForNumber(45) = %q %v", id, ok)
	}
	if got := p.Replies["draft-review"]; len(got) != 2 {
		t.Fatalf("replies %v", got)
	}
	if p.Hash == "" || p.Version != "1" {
		t.Fatalf("hash %q version %q", p.Hash, p.Version)
	}
	for _, d := range diags {
		if d.Severity == DiagError {
			t.Errorf("unexpected error diagnostic %+v", d)
		}
	}
}

// Each refusal from the RFC's "Deterministic" list, and the structural
// checks around them, produces its code and no Profile.
func TestCompileRefusals(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m *Model)
		code string
	}{
		{"no version", func(m *Model) { m.Spec.Meta.Version = "" }, CodeNoVersion},
		{"state without exit", func(m *Model) {
			var tr []Transition
			for _, x := range m.Spec.Transitions {
				if x.From != "approved" {
					tr = append(tr, x)
				}
			}
			m.Spec.Transitions = tr
		}, CodeNoExit},
		{"gate without owner", func(m *Model) { m.Spec.Gates[0].ReleasedBy = "" }, CodeGateNoOwner},
		{"gate with unknown role", func(m *Model) { m.Spec.Gates[0].ReleasedBy = "nobody" }, CodeUnknownRole},
		{"agent with no human gate after it", func(m *Model) {
			for i := range m.Spec.Roles {
				m.Spec.Roles[i].Human = false
			}
		}, CodeAgentNoHumanGate},
		{"agent in unknown state", func(m *Model) { m.Spec.Agents[0].States = []string{"ghost"} }, CodeUnknownState},
		{"two events on one fact", func(m *Model) { m.Spec.Events[1].Fact = m.Spec.Events[0].Fact }, CodeDuplicateEvent},
		{"state in no column", func(m *Model) { m.Spec.Columns[0].States = []string{"analysing"} }, CodeNoColumn},
		{"state in two columns", func(m *Model) { m.Spec.Columns[0].States = []string{"requested", "analysing"} }, CodeUnknownColumn},
		{"state names a column that does not collect it", func(m *Model) { m.Spec.States[0].Lifecycle.Column = "done" }, CodeUnknownColumn},
		{"column collects unknown state", func(m *Model) { m.Spec.Columns[0].States = append(m.Spec.Columns[0].States, "ghost") }, CodeUnknownState},
		{"empty column", func(m *Model) { m.Spec.Columns = append(m.Spec.Columns, Column{ID: "x", Label: "x"}) }, CodeColumnEmpty},
		{"no terminal column", func(m *Model) { m.Spec.Columns[4].Terminal = false }, CodeNoTerminalColumn},
		{"duplicate stage number", func(m *Model) { m.Spec.States[1].Lifecycle.Number = 10 }, CodeDuplicateNumber},
		{"unknown fact", func(m *Model) { m.Spec.Derivation.Rules[0].When[0].Fact = "weather" }, CodeUnknownFact},
		{"bad operator", func(m *Model) { m.Spec.Derivation.Rules[0].When[0].Op = OpHas }, CodeBadOp},
		{"scalar op without value", func(m *Model) { m.Spec.Derivation.Rules[2].When[0].Value = "" }, CodeBadOp},
		{"rules mode with no rules", func(m *Model) { m.Spec.Derivation.Rules = nil }, CodeNoRules},
		{"rule names unknown state", func(m *Model) { m.Spec.Derivation.Rules[0].State = "ghost" }, CodeUnknownState},
		{"bad derivation mode", func(m *Model) { m.Spec.Derivation.Mode = "oracle" }, CodeBadMode},
		{"transition on undeclared event", func(m *Model) { m.Spec.Transitions[0].On = "thunder" }, CodeUnknownEvent},
		{"transition needs unknown gate", func(m *Model) { m.Spec.Transitions[0].GateRef = "ghost" }, CodeUnknownGate},
		{"event actor is not a role", func(m *Model) { m.Spec.Events[0].Actor = "ghost" }, CodeUnknownRole},
		{"bookkeeping on undeclared event", func(m *Model) {
			m.Spec.Bookkeeping = []Effect{{On: "thunder", Action: "comment"}}
		}, CodeUnknownEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := techdocs(t)
			tc.mut(m)
			p, diags, err := Compile(m)
			if err == nil || p != nil {
				t.Fatalf("compiled; diags %+v", diags)
			}
			if !strings.Contains(err.Error(), ErrCompile.Error()) {
				t.Fatalf("err %v does not wrap ErrCompile", err)
			}
			found := false
			for _, d := range diags {
				if d.Code == tc.code && d.Severity == DiagError {
					found = true
				}
			}
			if !found {
				t.Fatalf("want %s in %+v", tc.code, diags)
			}
		})
	}
}

func TestCompileWarnings(t *testing.T) {
	m := techdocs(t)
	m.Spec.Gates[0].Confidence = ConfidenceInferred
	m.Spec.Events[0].Confidence = ConfidenceInferred
	p, diags, err := Compile(m)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range diags {
		if d.Code == CodeInferred && d.Severity == DiagWarning {
			n++
		}
	}
	if n != 2 || len(p.Diagnostics) != len(diags) {
		t.Fatalf("inferred warnings %d; profile carries %d of %d", n, len(p.Diagnostics), len(diags))
	}

	m = techdocs(t)
	m.Spec.Derivation.Mode = DerivationBuiltin
	if _, diags, err = Compile(m); err != nil {
		t.Fatal(err)
	}
	if len(diags) != 1 || diags[0].Code != CodeBadMode || diags[0].Severity != DiagWarning {
		t.Fatalf("builtin with rules: %+v", diags)
	}
}

func TestCompileInvalidModel(t *testing.T) {
	m := techdocs(t)
	m.Spec.Initial = "ghost"
	p, diags, err := Compile(m)
	if err == nil || p != nil || len(diags) != 1 || diags[0].Code != "invalid" {
		t.Fatalf("%v %+v", err, diags)
	}
}

// A closure edge out of an agent state is an observation, not a way for
// the agent to reach an outcome unsupervised; a merge edge is.
func TestCompileClosureEdgesAreNotAgentPaths(t *testing.T) {
	m := techdocs(t)
	if _, _, err := Compile(m); err != nil {
		t.Fatal(err)
	}
	m.Spec.Transitions = append(m.Spec.Transitions, Transition{From: "drafting", To: "published", On: "merged"})
	_, diags, err := Compile(m)
	if err == nil {
		t.Fatal("agent merging its own draft must be refused")
	}
	if !strings.Contains(err.Error(), CodeAgentNoHumanGate+" drafting") {
		t.Fatalf("%v %+v", err, diags)
	}
}

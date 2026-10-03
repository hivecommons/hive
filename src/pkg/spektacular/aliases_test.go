package spektacular

import (
	"testing"

	"github.com/hivecommons/hive/pkg/planengine"
)

// TestAliasesMatchPlanengine pins the type parity: the pkg/spektacular names
// are aliases, so values flow between the two packages without conversion.
func TestAliasesMatchPlanengine(t *testing.T) {
	var _ planengine.ArtifactStatus = ArtifactStatus{}
	var _ planengine.Plan = Plan{Tasks: []PlanTask{{Ref: "T1"}}}
	var _ planengine.DocumentStatus = DocumentFinal
	var _ error = (*planengine.NotFoundError)(&NotFoundError{})
	var _ error = (*planengine.VerbError)(&VerbError{})
	var _ error = (*planengine.ContractError)(&ContractError{})
	var _ error = (*planengine.WorkDirError)(&WorkDirError{})
	var _ error = (*planengine.PlanImportError)(&PlanImportError{})
	if DocumentStale != planengine.DocumentStale || DocumentSuperseded != planengine.DocumentSuperseded ||
		DocumentArchived != planengine.DocumentArchived || DocumentDraft != planengine.DocumentDraft {
		t.Fatal("document status constants diverged from planengine")
	}
}

// TestParsePlanJSON_BothTaskShapes keeps the engine-local task decoding: the
// neutral PlanTask has no UnmarshalJSON, so both export shapes go through
// planTaskWire.
func TestParsePlanJSON_BothTaskShapes(t *testing.T) {
	data := []byte(`{"kind":"plan","name":"demo","tasks":[
		{"ref":"T1","title":"old","repo":"o/r","execution":"human_required"},
		{"ref":"T2","title":"new","repo":{"name":"r","location":"https://github.com/o/r.git"},"execution":{"type":"agent_suitable","reason":"x"},"depends_on":["T1"]}
	]}`)
	plan, err := parsePlanJSON("demo", data, "plan export")
	if err != nil {
		t.Fatal(err)
	}
	want := []PlanTask{
		{Ref: "T1", Title: "old", Repo: "o/r", Execution: "human_required"},
		{Ref: "T2", Title: "new", Repo: "o/r", Execution: "agent_suitable", DependsOn: []string{"T1"}},
	}
	if len(plan.Tasks) != len(want) {
		t.Fatalf("tasks = %+v", plan.Tasks)
	}
	for i := range want {
		got := plan.Tasks[i]
		if got.Ref != want[i].Ref || got.Title != want[i].Title || got.Repo != want[i].Repo || got.Execution != want[i].Execution || len(got.DependsOn) != len(want[i].DependsOn) {
			t.Errorf("task %d = %+v, want %+v", i, got, want[i])
		}
	}
}

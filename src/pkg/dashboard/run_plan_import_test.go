package dashboard

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
)

func TestImportRunPlanEngineSource(t *testing.T) {
	for _, engine := range []string{"", "spektacular", "second-planner"} {
		t.Run("engine="+engine, func(t *testing.T) {
			_, s, store, _ := spekHub(t)
			var names []string
			want := "spektacular"
			if engine != "" {
				names = []string{engine}
				want = engine
			}
			if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers [agent_suitable]", names...); err != nil {
				t.Fatal(err)
			}
			_, epic := s.findRunEpic(spekRunKey)
			if epic == nil || epic.Meta(planning.MetaSource) != want || epic.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
				t.Fatalf("imported epic = %+v", epic)
			}
			tree, err := planning.GetPlanTree(store, epic.ID)
			if err != nil || len(tree.Children) != 1 {
				t.Fatalf("plan tree = %+v, err = %v", tree, err)
			}
			if err := s.ImportRunPlan(spekRunKey, spekRepo, "not a task list", names...); err != nil {
				t.Fatalf("repeat import: %v", err)
			}
			again, err := planning.GetPlanTree(store, epic.ID)
			if err != nil || !reflect.DeepEqual(tree, again) {
				t.Fatalf("repeat import changed plan: %+v, err = %v", again, err)
			}
		})
	}
}

func TestImportRunPlanLeavesOtherPlannerAlone(t *testing.T) {
	for _, owner := range []string{"spektacular", "another-planner"} {
		for _, hasPlan := range []bool{false, true} {
			t.Run(owner+"/planned="+map[bool]string{false: "false", true: "true"}[hasPlan], func(t *testing.T) {
				_, s, store, _ := spekHub(t)
				epic, err := store.Create("Existing run", beads.TypeEpic, beads.PriorityMedium, "architect", spekRunKey)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadata(epic.ID, planning.MetaSource, owner); err != nil {
					t.Fatal(err)
				}
				if hasPlan {
					epic, _ = store.Get(epic.ID)
					if _, err := planning.DecomposeFromOutput(store, epic, "1. [T1] Original task [agent_suitable]", planning.Options{AutoApprove: false}); err != nil {
						t.Fatal(err)
					}
				}
				before, err := json.Marshal(store.List(beads.ListFilter{}))
				if err != nil {
					t.Fatal(err)
				}
				if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T2] Replacement task [agent_suitable]", "second-planner"); err != nil {
					t.Fatal(err)
				}
				after, err := json.Marshal(store.List(beads.ListFilter{}))
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatalf("foreign planner's beads changed: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}

func TestImportRunPlanTagsReusedEpic(t *testing.T) {
	_, s, store, _ := spekHub(t)
	epic, err := store.Create("Bound run", beads.TypeEpic, beads.PriorityMedium, "architect", spekRunKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers [agent_suitable]", "second-planner"); err != nil {
		t.Fatal(err)
	}
	_, got := s.findRunEpic(spekRunKey)
	if got == nil || got.ID != epic.ID || got.Meta(planning.MetaSource) != "second-planner" || got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("reused epic = %+v", got)
	}
}

func TestImportRunPlanRejectsInvalidEngineNames(t *testing.T) {
	for _, names := range [][]string{{""}, {" "}, {"one", "two"}} {
		_, s, store, _ := spekHub(t)
		if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] Add helpers [agent_suitable]", names...); err == nil {
			t.Fatalf("accepted engine names %q", names)
		}
		if got := store.List(beads.ListFilter{}); len(got) != 0 {
			t.Fatalf("invalid engine names created beads: %+v", got)
		}
	}
}

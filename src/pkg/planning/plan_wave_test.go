package planning

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestPlanTreeWaveText(t *testing.T) {
	if (*PlanTree)(nil).WaveText() != "" {
		t.Fatal("nil tree must render empty")
	}
	store := newStore(t)
	epic, _ := decomposeEpic(t, store, "widget subsystem", Options{})
	if err := ApprovePlan(store, epic.ID); err != nil {
		t.Fatal(err)
	}
	tree, err := GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := tree.WaveText()
	if !strings.HasPrefix(got, "Plan: widget subsystem ("+epic.ID+", "+PlanStatusApproved+")\n") {
		t.Fatalf("header wrong:\n%s", got)
	}
	for _, want := range []string{"- [T1] Design the data model [agent_suitable] (open)", "- [T2] Get security review sign-off [human_required]", "depends: "} {
		if !strings.Contains(got, want) {
			t.Errorf("wave lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != len(tree.Children) {
		t.Fatalf("want one line per child plus the header:\n%s", got)
	}

	// An undecomposed epic renders its header and says so.
	bare := mustEpic(t, store, "bare")
	tree, _ = GetPlanTree(store, bare.ID)
	if got := tree.WaveText(); got != "Plan: bare ("+bare.ID+", not decomposed)" {
		t.Fatalf("undecomposed wave = %q", got)
	}
}

func TestFindPlanTree(t *testing.T) {
	store := newStore(t)
	epic, _ := decomposeEpic(t, store, "findable", Options{})
	if err := store.SetMetadata(epic.ID, MetaIssueRepo, "acme/hive"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(epic.ID, MetaIssueNumber, "42"); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*beads.Store{"architect": store, "empty": nil}

	if tree, ok := FindPlanTree(stores, epic.ID); !ok || tree.EpicID != epic.ID {
		t.Fatalf("by epic ID: ok=%v tree=%+v", ok, tree)
	}
	if tree, ok := FindPlanTree(stores, "acme/hive#42"); !ok || tree.EpicID != epic.ID {
		t.Fatalf("by issue ref: ok=%v tree=%+v", ok, tree)
	}
	for _, ref := range []string{"", "  ", "acme/hive#43", "no-such-epic"} {
		if _, ok := FindPlanTree(stores, ref); ok {
			t.Errorf("ref %q resolved to a plan", ref)
		}
	}
	// A plain task is not a plan, even by ID.
	task := mustTask(t, store, "task")
	if _, ok := FindPlanTree(stores, task.ID); ok {
		t.Fatal("task bead resolved as a plan")
	}
	if _, ok := FindPlanTree(nil, epic.ID); ok {
		t.Fatal("nil stores resolved a plan")
	}
}

package planning

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
)

// newStore returns a beads.Store rooted at a fresh temp dir.
func newStore(t *testing.T) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// mustEpic creates and returns an epic bead.
func mustEpic(t *testing.T, store *beads.Store, title string) *beads.Bead {
	t.Helper()
	b, err := store.Create(title, beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	return b
}

const samplePlan = `Here is the plan:
1. [T1] Design the data model [agent_suitable]
2. [T2] Get security review sign-off (human_required)
3. [T3] Implement persistence (depends: T1) [agent_suitable]
4. [T4] Wire it together (depends: T1, T3) [agent_suitable]
`

func TestDecomposeFromOutput_HappyPath(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Build the widget subsystem")

	res, err := DecomposeFromOutput(store, epic, samplePlan, Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if len(res.Children) != 4 {
		t.Fatalf("want 4 children, got %d", len(res.Children))
	}

	// Titles preserved and in order.
	wantTitles := []string{
		"Design the data model",
		"Get security review sign-off",
		"Implement persistence",
		"Wire it together",
	}
	for i, want := range wantTitles {
		if res.Children[i].Title != want {
			t.Errorf("child[%d] title = %q, want %q", i, res.Children[i].Title, want)
		}
		if res.Children[i].Type != beads.TypeTask {
			t.Errorf("child[%d] type = %q, want task", i, res.Children[i].Type)
		}
	}

	// Metadata: parent_epic on every child, execution tags correct.
	for _, c := range res.Children {
		got, err := store.Get(c.ID)
		if err != nil {
			t.Fatalf("get child: %v", err)
		}
		if got.Meta(MetaParentEpic) != epic.ID {
			t.Errorf("child %s parent_epic = %q, want %q", c.ID, got.Meta(MetaParentEpic), epic.ID)
		}
	}

	// Execution: T2 is human_required; the rest agent_suitable.
	if got := mustGet(t, store, res.Children[1].ID).Meta(MetaExecution); got != agentparse.ExecutionHumanRequired {
		t.Errorf("T2 execution = %q, want human_required", got)
	}
	for _, idx := range []int{0, 2, 3} {
		if got := mustGet(t, store, res.Children[idx].ID).Meta(MetaExecution); got != agentparse.ExecutionAgentSuitable {
			t.Errorf("child[%d] execution = %q, want agent_suitable", idx, got)
		}
	}

	// plan_ref recorded.
	if got := mustGet(t, store, res.Children[0].ID).Meta(MetaPlanRef); got != "T1" {
		t.Errorf("child[0] plan_ref = %q, want T1", got)
	}

	// Dependencies: T3 depends on T1; T4 depends on T1 and T3.
	t1, t3, t4 := res.Children[0].ID, res.Children[2].ID, res.Children[3].ID
	assertDeps(t, store, t3, []string{t1})
	assertDeps(t, store, t4, []string{t1, t3})

	// Epic marked plan_status=draft.
	if got := mustGet(t, store, epic.ID).Meta(MetaPlanStatus); got != PlanStatusDraft {
		t.Errorf("epic plan_status = %q, want draft", got)
	}
}

func TestDecomposeFromOutput_UnmarkedTaskDefaultsAgentSuitable(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Some epic")
	res, err := DecomposeFromOutput(store, epic, "1. [T1] a task with no execution tag", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if got := mustGet(t, store, res.Children[0].ID).Meta(MetaExecution); got != agentparse.ExecutionAgentSuitable {
		t.Errorf("execution = %q, want agent_suitable default", got)
	}
}

func TestDecomposeFromOutput_UnknownDepSkipped(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	// T1 depends on a ref (T9) that is never defined — must be skipped, not error.
	res, err := DecomposeFromOutput(store, epic, "1. [T1] only task (depends: T9)", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	assertDeps(t, store, res.Children[0].ID, nil)
}

func TestDecomposeFromOutput_SelfDepSkipped(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	res, err := DecomposeFromOutput(store, epic, "1. [T1] recursive (depends: T1)", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	assertDeps(t, store, res.Children[0].ID, nil)
}

func TestDecomposeFromOutput_NoTasks(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	_, err := DecomposeFromOutput(store, epic, "just prose, no list items here", Options{})
	if err == nil || !strings.Contains(err.Error(), "no tasks") {
		t.Fatalf("want no-tasks error, got %v", err)
	}
}

func TestDecomposeFromOutput_NonEpicRejected(t *testing.T) {
	store := newStore(t)
	task, err := store.Create("not an epic", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	_, err = DecomposeFromOutput(store, task, samplePlan, Options{})
	if err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("want non-epic rejection, got %v", err)
	}
}

func TestDecomposeFromOutput_NilEpic(t *testing.T) {
	store := newStore(t)
	if _, err := DecomposeFromOutput(store, nil, samplePlan, Options{}); err == nil {
		t.Fatal("want error for nil epic")
	}
}

func TestDecomposeFromOutput_EpicNotInStore(t *testing.T) {
	store := newStore(t)
	// An epic-typed bead that was never persisted to this store.
	ghost := &beads.Bead{ID: "ghost1234567", Title: "ghost", Type: beads.TypeEpic}
	if _, err := DecomposeFromOutput(store, ghost, samplePlan, Options{}); err == nil {
		t.Fatal("want error for epic not in store")
	}
}

func TestDecomposeFromOutput_ActorOverride(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	res, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{Actor: "quality"})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if res.Children[0].Actor != "quality" {
		t.Errorf("actor = %q, want quality (override)", res.Children[0].Actor)
	}
}

func TestDecomposeFromOutput_ChildPriorityOverride(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	pri := beads.PriorityCritical
	res, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{ChildPriority: &pri})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if res.Children[0].Priority != beads.PriorityCritical {
		t.Errorf("priority = %d, want critical", res.Children[0].Priority)
	}
}

func TestDecomposeFromOutput_InheritsEpicPriority(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic") // created with PriorityHigh
	res, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if res.Children[0].Priority != beads.PriorityHigh {
		t.Errorf("priority = %d, want inherited High", res.Children[0].Priority)
	}
}

func TestDecomposeFromOutput_OutOfRangePriorityDefaults(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	// Force an out-of-range priority on the epic to exercise the default clamp.
	if err := store.Update(epic.ID, func(b *beads.Bead) { b.Priority = beads.Priority(99) }); err != nil {
		t.Fatalf("update: %v", err)
	}
	epic = mustGet(t, store, epic.ID)
	res, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if res.Children[0].Priority != defaultChildPriority {
		t.Errorf("priority = %d, want default %d", res.Children[0].Priority, defaultChildPriority)
	}
}

func TestDeriveActor_ClassifierRoutesArchitect(t *testing.T) {
	store := newStore(t)
	// "refactor" routes to the architect lane via the classifier.
	epic := mustEpic(t, store, "Large refactor of the scheduler")
	res, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{})
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	if res.Children[0].Actor != string(classify.LaneArchitect) {
		t.Errorf("actor = %q, want architect", res.Children[0].Actor)
	}
}

func TestDeriveActor_FallsBackToArchitect(t *testing.T) {
	// A title with no lane keywords classifies to the default (scanner) lane,
	// so deriveActor falls back to the architect planner role.
	epic := &beads.Bead{ID: "x", Title: "plain unremarkable title", Type: beads.TypeEpic}
	if got := deriveActor(epic); got != architectActor {
		t.Errorf("deriveActor = %q, want architect fallback", got)
	}
}

func TestDeriveActor_UsesLabelsMetadata(t *testing.T) {
	epic := &beads.Bead{
		ID:    "x",
		Title: "unremarkable",
		Type:  beads.TypeEpic,
		Metadata: map[string]interface{}{
			"labels": "outreach, community",
		},
	}
	if got := deriveActor(epic); got != string(classify.LaneOutreach) {
		t.Errorf("deriveActor = %q, want outreach (from labels)", got)
	}
}

func TestEpicLabels(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]interface{}
		want []string
	}{
		{"none", nil, nil},
		{"empty string", map[string]interface{}{"labels": "  "}, nil},
		{"csv", map[string]interface{}{"labels": "a, b ,,c"}, []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epic := &beads.Bead{Metadata: tt.meta}
			got := epicLabels(epic)
			if len(got) != len(tt.want) {
				t.Fatalf("epicLabels = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("epicLabels[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuildPrompt(t *testing.T) {
	epic := &beads.Bead{
		ID:    "e1",
		Title: "Ship the reporting pipeline",
		Type:  beads.TypeEpic,
		Notes: "Must handle 1M events/day.",
	}
	p := BuildPrompt(epic)
	for _, want := range []string{
		"[agent:architect]",
		"Ship the reporting pipeline",
		"Must handle 1M events/day.",
		"agent_suitable",
		"human_required",
		"depends:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestBuildPrompt_BodyFromMetadata(t *testing.T) {
	epic := &beads.Bead{
		ID:    "e1",
		Title: "Epic",
		Type:  beads.TypeEpic,
		Metadata: map[string]interface{}{
			"description": "described in metadata",
		},
	}
	if !strings.Contains(BuildPrompt(epic), "described in metadata") {
		t.Error("prompt should include description metadata as body")
	}
}

func TestBuildPrompt_NoBody(t *testing.T) {
	epic := &beads.Bead{ID: "e1", Title: "Epic", Type: beads.TypeEpic}
	p := BuildPrompt(epic)
	if strings.Contains(p, "DETAILS:") {
		t.Error("prompt should omit DETAILS when there is no body")
	}
}

func TestEpicBody_Precedence(t *testing.T) {
	// Notes wins over metadata.
	epic := &beads.Bead{
		Notes:    "from notes",
		Metadata: map[string]interface{}{"body": "from body"},
	}
	if got := epicBody(epic); got != "from notes" {
		t.Errorf("epicBody = %q, want notes precedence", got)
	}
	// body metadata wins over description.
	epic2 := &beads.Bead{Metadata: map[string]interface{}{"body": "b", "description": "d"}}
	if got := epicBody(epic2); got != "b" {
		t.Errorf("epicBody = %q, want body", got)
	}
	// nothing -> empty.
	if got := epicBody(&beads.Bead{}); got != "" {
		t.Errorf("epicBody = %q, want empty", got)
	}
}

func TestDecompose_WithPlanner(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")

	var gotPrompt string
	planner := func(_ context.Context, prompt string) (string, error) {
		gotPrompt = prompt
		return samplePlan, nil
	}
	res, err := Decompose(context.Background(), store, epic, planner, Options{})
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(res.Children) != 4 {
		t.Fatalf("want 4 children, got %d", len(res.Children))
	}
	if !strings.Contains(gotPrompt, "[agent:architect]") {
		t.Error("planner was not given the architect prompt")
	}
}

func TestDecompose_NilPlanner(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	if _, err := Decompose(context.Background(), store, epic, nil, Options{}); err == nil {
		t.Fatal("want error for nil planner")
	}
}

func TestDecompose_PlannerError(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	boom := errors.New("boom")
	planner := func(_ context.Context, _ string) (string, error) { return "", boom }
	_, err := Decompose(context.Background(), store, epic, planner, Options{})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("want wrapped planner error, got %v", err)
	}
}

func TestDecompose_ValidatesEpicBeforePlanner(t *testing.T) {
	store := newStore(t)
	task, _ := store.Create("task", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	called := false
	planner := func(_ context.Context, _ string) (string, error) { called = true; return samplePlan, nil }
	if _, err := Decompose(context.Background(), store, task, planner, Options{}); err == nil {
		t.Fatal("want error for non-epic")
	}
	if called {
		t.Error("planner must not be called when epic is invalid")
	}
}

func TestDecompose_HonorsContextCancellation(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	planner := func(ctx context.Context, _ string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return samplePlan, nil
	}
	if _, err := Decompose(ctx, store, epic, planner, Options{}); err == nil {
		t.Fatal("want context-cancelled error")
	}
}

// TestDecomposeFromOutput_CreateChildError exercises the child-bead persistence
// error path by making the store directory read-only after the epic exists, so
// the first child create's WriteFile fails. Skipped when running as root (root
// bypasses filesystem permission checks).
func TestDecomposeFromOutput_CreateChildError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; filesystem permission enforcement is bypassed")
	}
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic := mustEpic(t, store, "Epic")

	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore permissions so t.TempDir cleanup can remove the directory.
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if _, err := DecomposeFromOutput(store, epic, "1. [T1] task", Options{}); err == nil {
		t.Fatal("want persistence error when store dir is read-only")
	} else if !strings.Contains(err.Error(), "creating child bead") {
		t.Errorf("error = %v, want creating-child-bead wrap", err)
	}
}

// ---- helpers ----

func mustGet(t *testing.T, store *beads.Store, id string) *beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func assertDeps(t *testing.T, store *beads.Store, id string, want []string) {
	t.Helper()
	b := mustGet(t, store, id)
	if len(b.DependsOn) != len(want) {
		t.Fatalf("%s deps = %v, want %v", id, b.DependsOn, want)
	}
	set := make(map[string]bool, len(b.DependsOn))
	for _, d := range b.DependsOn {
		set[d] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("%s missing dep %s (have %v)", id, w, b.DependsOn)
		}
	}
}

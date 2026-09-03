package planning

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
)

// mustTask creates a plain task bead (used for negative "not a child" cases).
func mustTask(t *testing.T, store *beads.Store, title string) *beads.Bead {
	t.Helper()
	b, err := store.Create(title, beads.TypeTask, beads.PriorityMedium, "worker", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return b
}

// decomposeEpic is a helper that decomposes an epic with the given options and
// returns the epic + result.
func decomposeEpic(t *testing.T, store *beads.Store, title string, opts Options) (*beads.Bead, *Result) {
	t.Helper()
	epic := mustEpic(t, store, title)
	res, err := DecomposeFromOutput(store, epic, samplePlan, opts)
	if err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	return epic, res
}

func TestApprovePlan_HappyPath(t *testing.T) {
	store := newStore(t)
	epic, _ := decomposeEpic(t, store, "Epic to approve", Options{})

	// Freshly decomposed → draft.
	got, _ := store.Get(epic.ID)
	if s := got.Meta(MetaPlanStatus); s != PlanStatusDraft {
		t.Fatalf("want draft after decompose, got %q", s)
	}

	if err := ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	got, _ = store.Get(epic.ID)
	if s := got.Meta(MetaPlanStatus); s != PlanStatusApproved {
		t.Fatalf("want approved, got %q", s)
	}
}

func TestApprovePlan_Errors(t *testing.T) {
	store := newStore(t)

	// Non-existent epic.
	if err := ApprovePlan(store, "nope"); err == nil {
		t.Fatal("want error for missing epic")
	}

	// Non-epic bead.
	task := mustTask(t, store, "a task")
	if err := ApprovePlan(store, task.ID); err == nil || !strings.Contains(err.Error(), "not an epic") {
		t.Fatalf("want non-epic error, got %v", err)
	}

	// Epic never decomposed (no plan_status).
	epic := mustEpic(t, store, "undecomposed")
	if err := ApprovePlan(store, epic.ID); err == nil || !strings.Contains(err.Error(), "no plan") {
		t.Fatalf("want no-plan error, got %v", err)
	}

	// Already approved.
	epic2, _ := decomposeEpic(t, store, "already approved", Options{})
	if err := ApprovePlan(store, epic2.ID); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if err := ApprovePlan(store, epic2.ID); err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Fatalf("want already-approved error, got %v", err)
	}

	// Nil store.
	if err := ApprovePlan(nil, "x"); err == nil {
		t.Fatal("want error for nil store")
	}
}

func TestRejectPlan(t *testing.T) {
	store := newStore(t)
	epic, _ := decomposeEpic(t, store, "reject me", Options{})
	if err := ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := RejectPlan(store, epic.ID); err != nil {
		t.Fatalf("RejectPlan: %v", err)
	}
	got, _ := store.Get(epic.ID)
	if s := got.Meta(MetaPlanStatus); s != PlanStatusDraft {
		t.Fatalf("want draft after reject, got %q", s)
	}

	// Errors: missing, non-epic, undecomposed.
	if err := RejectPlan(store, "missing"); err == nil {
		t.Fatal("want error for missing epic")
	}
	task := mustTask(t, store, "task")
	if err := RejectPlan(store, task.ID); err == nil {
		t.Fatal("want error for non-epic")
	}
	undec := mustEpic(t, store, "undecomposed")
	if err := RejectPlan(store, undec.ID); err == nil {
		t.Fatal("want error for undecomposed epic")
	}
}

func TestGetPlanTree(t *testing.T) {
	store := newStore(t)
	epic, res := decomposeEpic(t, store, "tree epic", Options{})

	tree, err := GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatalf("GetPlanTree: %v", err)
	}
	if tree.EpicID != epic.ID || tree.EpicTitle != "tree epic" {
		t.Fatalf("epic identity wrong: %+v", tree)
	}
	if tree.PlanStatus != PlanStatusDraft || tree.Approved {
		t.Fatalf("want draft/not-approved, got status=%q approved=%v", tree.PlanStatus, tree.Approved)
	}
	if len(tree.Children) != len(res.Children) {
		t.Fatalf("want %d children, got %d", len(res.Children), len(tree.Children))
	}
	// Execution tags carried; the human_required task from samplePlan present.
	var sawHuman, sawDeps bool
	for _, c := range tree.Children {
		if c.Execution == agentparse.ExecutionHumanRequired {
			sawHuman = true
		}
		if len(c.DependsOn) > 0 {
			sawDeps = true
		}
		if c.PlanRef == "" {
			t.Errorf("child %s missing plan ref", c.ID)
		}
	}
	if !sawHuman {
		t.Error("expected a human_required child")
	}
	if !sawDeps {
		t.Error("expected at least one dependency edge")
	}

	// After approve, tree reflects it.
	if err := ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	tree, _ = GetPlanTree(store, epic.ID)
	if !tree.Approved || tree.PlanStatus != PlanStatusApproved {
		t.Fatalf("want approved tree, got %+v", tree)
	}
}

func TestGetPlanTree_Errors(t *testing.T) {
	store := newStore(t)
	if _, err := GetPlanTree(store, "missing"); err == nil {
		t.Fatal("want error for missing epic")
	}
	task := mustTask(t, store, "task")
	if _, err := GetPlanTree(store, task.ID); err == nil {
		t.Fatal("want error for non-epic")
	}
	// Undecomposed epic → empty status, no children, no error.
	epic := mustEpic(t, store, "undecomposed")
	tree, err := GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatalf("undecomposed epic should not error: %v", err)
	}
	if tree.PlanStatus != "" || len(tree.Children) != 0 {
		t.Fatalf("want empty tree, got %+v", tree)
	}
}

func TestRetagChild(t *testing.T) {
	store := newStore(t)
	epic, res := decomposeEpic(t, store, "retag epic", Options{})
	child := res.Children[0]

	if err := RetagChild(store, epic.ID, child.ID, agentparse.ExecutionHumanRequired); err != nil {
		t.Fatalf("RetagChild: %v", err)
	}
	got, _ := store.Get(child.ID)
	if e := got.Meta(MetaExecution); e != agentparse.ExecutionHumanRequired {
		t.Fatalf("want human_required, got %q", e)
	}

	// Invalid execution value.
	if err := RetagChild(store, epic.ID, child.ID, "bogus"); err == nil {
		t.Fatal("want error for invalid execution")
	}
	// Missing child.
	if err := RetagChild(store, epic.ID, "missing", agentparse.ExecutionAgentSuitable); err == nil {
		t.Fatal("want error for missing child")
	}
	// Bead that is not a child of this epic.
	other := mustTask(t, store, "orphan")
	if err := RetagChild(store, epic.ID, other.ID, agentparse.ExecutionAgentSuitable); err == nil {
		t.Fatal("want error for non-child bead")
	}
}

func TestRemoveChild(t *testing.T) {
	store := newStore(t)
	epic, res := decomposeEpic(t, store, "remove epic", Options{})
	child := res.Children[0]

	if err := RemoveChild(store, epic.ID, child.ID); err != nil {
		t.Fatalf("RemoveChild: %v", err)
	}
	got, _ := store.Get(child.ID)
	if got.Status != beads.StatusClosed {
		t.Fatalf("want closed after remove, got %q", got.Status)
	}

	// Missing child + non-child bead.
	if err := RemoveChild(store, epic.ID, "missing"); err == nil {
		t.Fatal("want error for missing child")
	}
	other := mustTask(t, store, "orphan")
	if err := RemoveChild(store, epic.ID, other.ID); err == nil {
		t.Fatal("want error for non-child bead")
	}
	// Nil store guards.
	if err := verifyChild(nil, epic.ID, child.ID); err == nil {
		t.Fatal("want error for nil store in verifyChild")
	}
}

// TestPlanReview_PersistErrors exercises the SetMetadata/Close persistence
// error branches of the review transitions by making the store dir read-only
// after the plan exists in memory. Skipped as root (perms bypassed).
func TestPlanReview_PersistErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; filesystem permission enforcement is bypassed")
	}
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic := mustEpic(t, store, "persist epic")
	res, err := DecomposeFromOutput(store, epic, samplePlan, Options{})
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	child := res.Children[0]

	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if err := ApprovePlan(store, epic.ID); err == nil {
		t.Error("ApprovePlan: want persist error")
	}
	// After a failed approve the status may have flipped in memory; force draft
	// so RejectPlan reaches its SetMetadata call.
	_ = store.Update(epic.ID, func(b *beads.Bead) { b.Metadata[MetaPlanStatus] = PlanStatusDraft })
	if err := RejectPlan(store, epic.ID); err == nil {
		t.Error("RejectPlan: want persist error")
	}
	if err := RetagChild(store, epic.ID, child.ID, agentparse.ExecutionHumanRequired); err == nil {
		t.Error("RetagChild: want persist error")
	}
	if err := RemoveChild(store, epic.ID, child.ID); err == nil {
		t.Error("RemoveChild: want persist error")
	}
}

func TestDecompose_AutoApprove(t *testing.T) {
	store := newStore(t)
	epic, _ := decomposeEpic(t, store, "auto approved", Options{AutoApprove: true})
	got, _ := store.Get(epic.ID)
	if s := got.Meta(MetaPlanStatus); s != PlanStatusApproved {
		t.Fatalf("want approved with AutoApprove, got %q", s)
	}
}

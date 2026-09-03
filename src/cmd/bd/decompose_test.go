package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
)

// decomposeSamplePlan is a plan in the exact shape agentparse.ParseTaskList
// consumes (mirrors pkg/planning's fixtures).
const decomposeSamplePlan = `Here is the plan:
1. [T1] Design the data model [agent_suitable]
2. [T2] Implement persistence (depends: T1) [agent_suitable]
3. [T3] Wire it together (depends: T1, T2) [agent_suitable]
`

// newDecomposeEpic points BD_DIR at a fresh temp store and creates an epic in
// it, returning the store dir and the epic. cmdDecompose re-opens its own
// store via openStore(), so BD_DIR is the seam that redirects it.
func newDecomposeEpic(t *testing.T) (string, *beads.Bead) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BD_DIR", dir)
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("Build the widget subsystem", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	return dir, epic
}

// --- readPlan ---

func TestReadPlanFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(path, []byte("1. [T1] Do the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readPlan(path)
	if err != nil {
		t.Fatalf("readPlan(%q): %v", path, err)
	}
	if got != "1. [T1] Do the thing\n" {
		t.Errorf("readPlan = %q; want file contents", got)
	}
}

func TestReadPlanFileNotFound(t *testing.T) {
	if _, err := readPlan(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("readPlan(missing file) = nil error; want error")
	}
}

func TestReadPlanFromStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if _, err := w.WriteString("stdin plan body"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	got, err := readPlan("")
	if err != nil {
		t.Fatalf("readPlan(\"\"): %v", err)
	}
	if got != "stdin plan body" {
		t.Errorf("readPlan from stdin = %q; want %q", got, "stdin plan body")
	}
}

// --- decomposePromptFor ---

func TestDecomposePromptForWrapsBuildPrompt(t *testing.T) {
	epic := &beads.Bead{ID: "epic12345678", Title: "Ship the frobnicator", Type: beads.TypeEpic}
	got := decomposePromptFor(epic)
	if got != planning.BuildPrompt(epic) {
		t.Error("decomposePromptFor should be a thin wrapper over planning.BuildPrompt")
	}
	if !strings.Contains(got, "Ship the frobnicator") {
		t.Errorf("prompt does not mention the epic title:\n%s", got)
	}
}

// --- cmdDecompose ---

func TestCmdDecomposePrintPrompt(t *testing.T) {
	_, epic := newDecomposeEpic(t)

	out := captureStdout(t, func() {
		cmdDecompose([]string{epic.ID, "--print-prompt"})
	})
	if out != planning.BuildPrompt(epic) {
		t.Errorf("--print-prompt output differs from planning.BuildPrompt:\n%s", out)
	}
}

func TestCmdDecomposeFromPlanFile(t *testing.T) {
	dir, epic := newDecomposeEpic(t)
	planPath := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(planPath, []byte(decomposeSamplePlan), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdDecompose([]string{epic.ID, "--plan", planPath})
	})

	if !strings.Contains(out, "into 3 child bead(s)") {
		t.Errorf("output missing child count:\n%s", out)
	}
	if !strings.Contains(out, "plan_status="+planning.PlanStatusDraft) {
		t.Errorf("default run should report draft plan_status:\n%s", out)
	}
	for _, title := range []string{"Design the data model", "Implement persistence", "Wire it together"} {
		if !strings.Contains(out, title) {
			t.Errorf("output missing child title %q:\n%s", title, out)
		}
	}

	// cmdDecompose persisted through its own store; re-open to observe disk.
	store := reloadStore(t, dir)
	epicNow, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("reload epic: %v", err)
	}
	if got := epicNow.Meta(planning.MetaPlanStatus); got != planning.PlanStatusDraft {
		t.Errorf("epic plan_status = %q; want %q", got, planning.PlanStatusDraft)
	}
	all := store.List(beads.ListFilter{})
	tasks := 0
	for _, b := range all {
		if b.Type == beads.TypeTask {
			tasks++
		}
	}
	if tasks != 3 {
		t.Errorf("persisted %d task beads; want 3", tasks)
	}
}

func TestCmdDecomposeAutoApprove(t *testing.T) {
	dir, epic := newDecomposeEpic(t)
	planPath := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(planPath, []byte(decomposeSamplePlan), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdDecompose([]string{epic.ID, "--plan", planPath, "--auto-approve"})
	})
	if !strings.Contains(out, "plan_status="+planning.PlanStatusApproved) {
		t.Errorf("--auto-approve should report approved plan_status:\n%s", out)
	}

	store := reloadStore(t, dir)
	epicNow, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("reload epic: %v", err)
	}
	if got := epicNow.Meta(planning.MetaPlanStatus); got != planning.PlanStatusApproved {
		t.Errorf("epic plan_status = %q; want %q", got, planning.PlanStatusApproved)
	}
}

func TestCmdDecomposeFromStdin(t *testing.T) {
	dir, epic := newDecomposeEpic(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if _, err := w.WriteString(decomposeSamplePlan); err != nil {
		t.Fatal(err)
	}
	w.Close()

	out := captureStdout(t, func() {
		cmdDecompose([]string{epic.ID})
	})
	if !strings.Contains(out, "into 3 child bead(s)") {
		t.Errorf("stdin plan run missing child count:\n%s", out)
	}

	store := reloadStore(t, dir)
	all := store.List(beads.ListFilter{})
	tasks := 0
	for _, b := range all {
		if b.Type == beads.TypeTask {
			tasks++
		}
	}
	if tasks != 3 {
		t.Errorf("persisted %d task beads; want 3", tasks)
	}
}

func TestCmdDecomposeActorOverride(t *testing.T) {
	dir, epic := newDecomposeEpic(t)
	planPath := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(planPath, []byte(decomposeSamplePlan), 0o644); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		cmdDecompose([]string{epic.ID, "--plan", planPath, "--actor", "custom-lane"})
	})

	store := reloadStore(t, dir)
	all := store.List(beads.ListFilter{})
	for _, b := range all {
		if b.Type == beads.TypeTask && b.Actor != "custom-lane" {
			t.Errorf("child %s actor = %q; want custom-lane", b.ID, b.Actor)
		}
	}
}

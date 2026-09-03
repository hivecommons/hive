package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

func newPlanTestStore(t *testing.T) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("beads.NewStore: %v", err)
	}
	return store
}

func planLabeledIssue(number int, labels ...string) github.Issue {
	return github.Issue{Repo: "hivecommons/hive", Number: number, Title: "build the thing", Labels: labels}
}

// Below the planning ACMM floor the `plan` label must mint NOTHING — the
// architect that decomposes epics is not scheduled below L5, so an epic
// minted here would sit in decompose_pending forever. The nil governor makes
// the gate self-enforcing: any kick recorded past it would panic.
func TestPlanFromLabeledIssuesGatedBelowPlanningLevel(t *testing.T) {
	store := newPlanTestStore(t)
	stores := map[string]*beads.Store{planning.ArchitectAgentName: store}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		planLabeledIssue(1, "plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel-1)

	if got := len(store.List(beads.ListFilter{})); got != 0 {
		t.Errorf("minted %d epics below the planning ACMM floor, want 0", got)
	}
}

// At the planning floor a plan-labeled issue mints an epic INTO THE
// ARCHITECT'S STORE (never a sibling agent's), while unlabeled issues mint
// nothing. With no architect agent registered the plan queues rather than
// kicks — which the nil governor again enforces by construction.
func TestPlanFromLabeledIssuesRoutesToArchitectStore(t *testing.T) {
	architectStore := newPlanTestStore(t)
	otherStore := newPlanTestStore(t)
	stores := map[string]*beads.Store{
		planning.ArchitectAgentName: architectStore,
		"scanner":                   otherStore,
	}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: []github.Issue{
		planLabeledIssue(1, "plan"),
		planLabeledIssue(2, "kind/bug"),
		planLabeledIssue(3),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel)

	if got := len(architectStore.List(beads.ListFilter{})); got != 1 {
		t.Errorf("architect store holds %d beads, want 1 (only the plan-labeled issue mints)", got)
	}
	if got := len(otherStore.List(beads.ListFilter{})); got != 0 {
		t.Errorf("sibling agent store holds %d beads, want 0", got)
	}
}

// Nil enumeration and an empty store map are quiet no-ops — the eval cycle
// runs this every tick, including before the first GitHub pass and on hives
// with no bead stores at all.
func TestPlanFromLabeledIssuesNilInputs(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	planFromLabeledIssues(nil, map[string]*beads.Store{}, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel)
	planFromLabeledIssues(&github.ActionableResult{}, nil, mgr, nil, nil, restoreTestLogger(),
		planning.PlanningMinACMMLevel)
}

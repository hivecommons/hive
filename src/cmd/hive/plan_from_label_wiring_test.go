package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

func planLabelTestConfig() *config.Config {
	v := true
	return &config.Config{Planning: config.PlanningConfig{PlanFromLabel: &v}}
}

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
		planLabeledIssue(1, "hive-plan"),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
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
		planLabeledIssue(1, "hive-plan"),
		planLabeledIssue(2, "kind/bug"),
		planLabeledIssue(3),
	}}}

	planFromLabeledIssues(actionable, stores, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
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
	planFromLabeledIssues(nil, map[string]*beads.Store{}, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
		planning.PlanningMinACMMLevel)
	planFromLabeledIssues(&github.ActionableResult{}, nil, mgr, nil, nil, restoreTestLogger(), planLabelTestConfig(),
		planning.PlanningMinACMMLevel)
}

// With runs.spektacular enabled and a dashboard server present, none of the
// enumerated issues carrying a design label, planFromLabeledIssues must not
// compact actionable.Issues.Items in place: that slice is already published
// to lastActionable/dashboard readers (refreshDashboard, BuildFrontendStatus)
// which iterate it concurrently on the eval goroutine. Regression test for
// the issues[:0] aliasing bug (hivecommons/hive#9174).
func TestPlanFromLabeledIssuesDoesNotCorruptActionableItemsWithSpektacular(t *testing.T) {
	store := newPlanTestStore(t)
	stores := map[string]*beads.Store{planning.ArchitectAgentName: store}
	mgr := agent.NewManager(map[string]config.AgentConfig{}, restoreTestLogger(), agent.ProjectContext{})
	dashSrv := dashboard.NewServer(0, restoreTestLogger())

	original := []github.Issue{
		planLabeledIssue(1, "hive-plan"),
		planLabeledIssue(2, "kind/bug"),
		planLabeledIssue(3),
	}
	items := make([]github.Issue, len(original))
	copy(items, original)
	actionable := &github.ActionableResult{Issues: github.IssueResult{Items: items}}

	v := true
	cfg := &config.Config{Planning: config.PlanningConfig{PlanFromLabel: &v}}
	cfg.Runs.Spektacular.Enabled = true

	planFromLabeledIssues(actionable, stores, mgr, nil, dashSrv, restoreTestLogger(), cfg,
		planning.PlanningMinACMMLevel)

	if len(actionable.Issues.Items) != len(original) {
		t.Fatalf("actionable.Issues.Items len changed: got %d, want %d", len(actionable.Issues.Items), len(original))
	}
	for i, want := range original {
		got := actionable.Issues.Items[i]
		if got.Number != want.Number {
			t.Errorf("actionable.Issues.Items[%d].Number = %d, want %d (slice corrupted in place)", i, got.Number, want.Number)
		}
	}
}

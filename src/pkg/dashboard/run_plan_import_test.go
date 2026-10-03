package dashboard

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

// run_plan_import_test.go covers how a Spektacular plan lands on a run epic:
// design-mode epics minted as drafts still import (#10048), a regenerated plan
// replaces the old one, and an owner reset to plan re-opens review (#10064).

func openRunPlanChildCount(store *beads.Store, epicID string) int {
	n := 0
	for _, child := range runPlanChildren(store, epicID) {
		if child.Status != beads.StatusClosed {
			n++
		}
	}
	return n
}

func TestImportRunPlanIntoDesignModeEpic(t *testing.T) {
	_, s, store, _ := spekHub(t)
	epic, err := planning.EpicFromIssue(store, github.Issue{Repo: spekRepo, Number: 42, Title: "design me"}, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	if err := planning.RequestDesign(store, epic.ID); err != nil {
		t.Fatalf("RequestDesign: %v", err)
	}
	for key, value := range map[string]string{
		planning.MetaDesignVia: planning.DesignViaSpektacular,
		planning.MetaRunKey:    spekRunKey,
	} {
		if err := store.SetMetadata(epic.ID, key, value); err != nil {
			t.Fatalf("tag design epic: %v", err)
		}
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]\n2. [T2] y [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	if got := openRunPlanChildCount(store, epic.ID); got != 2 {
		t.Fatalf("children after import = %d, want 2", got)
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft || planning.DecomposePending(got) {
		t.Fatalf("epic after import: plan_status=%q decompose_pending=%v", got.Meta(planning.MetaPlanStatus), planning.DecomposePending(got))
	}
}

func TestImportRunPlanReplacesRegeneratedPlan(t *testing.T) {
	_, s, store, _ := spekHub(t)
	first := "1. [T1] x [agent_suitable]"
	if err := s.ImportRunPlan(spekRunKey, spekRepo, first); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("plan import did not create the epic")
	}
	old := runPlanChildren(store, epic.ID)
	if len(old) != 1 {
		t.Fatalf("children after first import = %d, want 1", len(old))
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, first); err != nil {
		t.Fatalf("repeat import: %v", err)
	}
	if !s.runPlanApproved(spekRunKey) || len(runPlanChildren(store, epic.ID)) != 1 {
		t.Fatal("repeat import of the same plan was not a no-op")
	}

	if err := s.ImportRunPlan(spekRunKey, spekRepo, "not a task list"); err == nil {
		t.Fatal("import of an unparseable regenerated plan succeeded")
	}
	if got, _ := store.Get(old[0].ID); got.Status == beads.StatusClosed {
		t.Fatal("failed re-import retired the previous plan")
	}

	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] a [agent_suitable]\n2. [T2] b [agent_suitable]"); err != nil {
		t.Fatalf("regenerated import: %v", err)
	}
	if s.runPlanApproved(spekRunKey) {
		t.Fatal("regenerated plan kept the old approval")
	}
	if got, _ := store.Get(old[0].ID); got.Status != beads.StatusClosed {
		t.Fatalf("superseded child status = %q, want closed", got.Status)
	}
	if got := openRunPlanChildCount(store, epic.ID); got != 2 {
		t.Fatalf("open children after regenerated import = %d, want 2", got)
	}
}

func TestImportRunPlanLeavesForeignPlanAlone(t *testing.T) {
	_, s, store, _ := spekHub(t)
	epic := spekDraftEpic(t, store)
	child, err := store.Create("foreign", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.SetMetadata(child.ID, planning.MetaParentEpic, epic.ID); err != nil {
		t.Fatalf("tag child: %v", err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	if got := runPlanChildren(store, epic.ID); len(got) != 1 || got[0].ID != child.ID {
		t.Fatalf("foreign plan was replaced: %+v", got)
	}
}

func TestRunResetToPlanReopensApprovedRunPlan(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("bead store: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	if err := s.ImportRunPlan(runResetTestKey, "myorg/repo1", "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(runResetTestKey)
	if epic == nil {
		t.Fatal("plan import did not create the epic")
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	recordRunResetLease(t, s, StageImplement, time.Now())

	rec := doOwnerPost(s, runResetTestPath, runResetRequest{To: StagePlan, Reason: "replan"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST reset = %d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("plan_status after reset = %q, want draft", got.Meta(planning.MetaPlanStatus))
	}
	if got.Meta(runPlanDigestMeta) != runPlanDigestReplan {
		t.Fatalf("import digest after reset = %q, want %q", got.Meta(runPlanDigestMeta), runPlanDigestReplan)
	}
	if !s.stageCheckpointHolds(runResetTestKey, StagePlan, StageImplement) {
		t.Fatal("re-run plan stage is not held at the checkpoint after reset")
	}
	old := runPlanChildren(store, epic.ID)
	if err := s.ImportRunPlan(runResetTestKey, "myorg/repo1", "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("re-import after reset: %v", err)
	}
	if got, _ := store.Get(old[0].ID); got.Status != beads.StatusClosed {
		t.Fatalf("pre-reset child status = %q, want closed", got.Status)
	}
	if got := openRunPlanChildCount(store, epic.ID); got != 1 {
		t.Fatalf("open children after re-import = %d, want 1", got)
	}
	if err := store.SetMetadata(epic.ID, runPlanDigestMeta, ""); err != nil {
		t.Fatalf("clear digest: %v", err)
	}
	if err := s.resetRunPlanForReplan(runResetTestKey); err != nil {
		t.Fatalf("replan reset without a digest: %v", err)
	}
	if err := s.resetRunPlanForReplan("unknown-run"); err != nil {
		t.Fatalf("replan reset of an unknown run: %v", err)
	}
}

func TestPopulateRunBurndownWithoutRequest(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.RunBurndown = func(ctx context.Context, key string) (*RunBurndown, error) {
		if ctx == nil {
			t.Fatal("nil context")
		}
		return &RunBurndown{Source: "wavefront", Scope: 3}, nil
	}
	run := Run{Key: spekRunKey}
	if err := s.populateRunBurndown(nil, &run); err != nil {
		t.Fatalf("populateRunBurndown: %v", err)
	}
	if run.Burndown == nil || run.Burndown.Scope != 3 {
		t.Fatalf("burndown = %+v", run.Burndown)
	}
}

func TestSpecCheckpointPayloadWithRunBurndown(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	spekDesignEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}
	s.deps.RunBurndown = func(context.Context, string) (*RunBurndown, error) { return nil, nil }
	payload, err := s.RunCheckpointPayload(spekRepo + "!" + spekRunKey + ":" + StageSpec)
	if err != nil {
		t.Fatalf("spec checkpoint payload: %v", err)
	}
	if payload.Stage != StageSpec {
		t.Fatalf("checkpoint stage = %s, want spec", payload.Stage)
	}
}

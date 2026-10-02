package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

// spekDesignRunAtPlanCheckpoint drives a design-mode run with the Spec
// checkpoint disabled across spec→plan, then parks its plan at the (default
// blocking) plan checkpoint.
func spekDesignRunAtPlanCheckpoint(t *testing.T) (*ContributeWSHub, *Server, *beads.Store, *beads.Bead) {
	t.Helper()
	hub, s, store, _ := spekHub(t)
	off := false
	s.deps.Config.Runs.Checkpoints.Spec = &off
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("spec advance with checkpoint disabled: %v", err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held plan advance: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stage before plan decision = %s, want plan", stage)
	}
	return hub, s, store, epic
}

func planCheckpoint(t *testing.T, s *Server) RunCheckpointPayload {
	t.Helper()
	runs, err := s.activeRuns(true)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %d, %v", len(runs), err)
	}
	payload, err := s.RunCheckpointPayload(runs[0].Key)
	if err != nil {
		t.Fatalf("plan checkpoint payload: %v", err)
	}
	if payload.Stage != StagePlan {
		t.Fatalf("checkpoint stage = %s, want plan", payload.Stage)
	}
	return payload
}

func TestDisabledSpecCheckpointApprovesDesign(t *testing.T) {
	_, _, store, epic := spekDesignRunAtPlanCheckpoint(t)
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) != planning.DesignStatusApproved {
		t.Fatalf("design status after spec checkpoint skipped = %q, want approved", planning.DesignStatus(got))
	}
}

// A design epic left pending at the plan checkpoint (runs from before the
// disabled Spec checkpoint approved the design) must still get a plan
// decision, not a design one that answers 200 without moving anything.
func TestPlanCheckpointApproveIgnoresPendingDesign(t *testing.T) {
	hub, s, store, epic := spekDesignRunAtPlanCheckpoint(t)
	if err := store.SetMetadata(epic.ID, planning.MetaDesignStatus, planning.DesignStatusRequested); err != nil {
		t.Fatalf("reset design status: %v", err)
	}
	payload := planCheckpoint(t, s)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(payload.RunKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: payload.Gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve plan checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("plan status after approve = %q, want approved", got.Meta(planning.MetaPlanStatus))
	}
	if stage, _ := spekLeaseState(hub); stage != StageImplement {
		t.Fatalf("stage after plan approve = %s, want implement", stage)
	}
}

func TestPlanCheckpointRejectIgnoresPendingDesign(t *testing.T) {
	hub, s, store, epic := spekDesignRunAtPlanCheckpoint(t)
	if err := store.SetMetadata(epic.ID, planning.MetaDesignStatus, planning.DesignStatusRequested); err != nil {
		t.Fatalf("reset design status: %v", err)
	}
	payload := planCheckpoint(t, s)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(payload.RunKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: payload.Gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject plan checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) != planning.DesignStatusRequested {
		t.Fatalf("design status after plan reject = %q, want requested (untouched)", planning.DesignStatus(got))
	}
	if stage, gen := spekLeaseState(hub); stage != StagePlan || gen <= payload.Gen {
		t.Fatalf("lease after design-run plan reject = %s gen %d, want plan past gen %d", stage, gen, payload.Gen)
	}
}

// A forge failure on the design-approved signal must not wedge the run: the
// lease crosses to plan, the design is recorded approved, and the failure lands
// on the run's timeline instead of a 502 that no retry can recover from.
func TestSpecCheckpointApproveSurvivesFailedDesignSignal(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := store.SetMetadata(epic.ID, planning.MetaIssueNumber, "8618"); err != nil {
		t.Fatalf("set issue number: %v", err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	}))
	t.Cleanup(gh.Close)
	s.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "myorg", []string{"repo1"}, s.logger)

	checkpointKey := spekRepo + "!" + spekRunKey + ":" + StageSpec
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(checkpointKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: spekGen})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve spec checkpoint with failing forge = %d body=%s", rec.Code, rec.Body.String())
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stage after spec approve = %s, want plan", stage)
	}
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) != planning.DesignStatusApproved {
		t.Fatalf("design status = %q, want approved", planning.DesignStatus(got))
	}
	found := false
	for _, ev := range s.LifecycleTimeline().ByIssue(spekRunKey) {
		if ev.Kind == timeline.KindProgress && ev.Attrs[stageAttrReason] == "design_signal_failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("failed design signal not recorded on the run timeline")
	}
}

// Approving a spec checkpoint whose lease has already moved on must refuse,
// never answer 200 without a transition.
func TestSpecCheckpointApproveRefusesWithoutHeldLease(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}
	payload, err := s.RunCheckpointPayload(spekRepo + "!" + spekRunKey + ":" + StageSpec)
	if err != nil {
		t.Fatalf("spec checkpoint payload: %v", err)
	}
	if _, err := hub.advanceLeaseStage(spekIdentity, spekTaskID, StagePlan, now); err != nil {
		t.Fatalf("move lease past spec: %v", err)
	}
	if _, err := s.advanceCheckpointLease(payload.RunKey, epic.ID, StageSpec, StagePlan, payload.Gen, "owner", now); err == nil {
		t.Fatal("advancing a spec checkpoint with no held spec lease succeeded")
	}
}

// Rejecting a held plan of a run-imported epic (run key, no issue number)
// re-mints the plan generation instead of leaving the run parked behind the
// rejected receipt (hivecommons/hive#10063).
func TestPlanCheckpointRejectRetriesHeldPlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic := spekDraftEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held plan advance: %v", err)
	}
	payload := planCheckpoint(t, s)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(payload.RunKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: payload.Gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject plan checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	stage, gen := spekLeaseState(hub)
	if stage != StagePlan || gen <= spekGen {
		t.Fatalf("lease after plan reject = %s gen %d, want plan past gen %d", stage, gen, spekGen)
	}
	if s.runCheckpointStageHeld(spekRunKey, StagePlan, gen) {
		t.Fatal("retried plan generation is still held")
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("plan status after reject = %q, want draft", got.Meta(planning.MetaPlanStatus))
	}
}

// A plan still being drafted has no receipt to reject past: the lease keeps
// its generation.
func TestPlanRejectLeavesUnheldPlanLease(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	spekLease(t, hub, StagePlan, time.Now())
	epic := spekDraftEpic(t, store)

	if err := s.resetRunLeaseAfterPlanReject(store, epic.ID); err != nil {
		t.Fatalf("resetRunLeaseAfterPlanReject: %v", err)
	}
	if stage, gen := spekLeaseState(hub); stage != StagePlan || gen != spekGen {
		t.Fatalf("lease after reject of unheld plan = %s gen %d, want plan gen %d", stage, gen, spekGen)
	}
}

// POST /api/plan/{epicID}/reject shares the plan checkpoint's reject contract:
// a held plan has its generation re-minted (hivecommons/hive#10063).
func TestPlanEndpointRejectRetriesHeldPlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic := spekDraftEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held plan advance: %v", err)
	}

	rec := doOwnerPost(s, "/api/plan/"+epic.ID+"/reject", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject plan = %d body=%s", rec.Code, rec.Body.String())
	}
	stage, gen := spekLeaseState(hub)
	if stage != StagePlan || gen <= spekGen {
		t.Fatalf("lease after plan reject = %s gen %d, want plan past gen %d", stage, gen, spekGen)
	}
	if s.runCheckpointStageHeld(spekRunKey, StagePlan, gen) {
		t.Fatal("retried plan generation is still held")
	}
}

// The checkpoint reject acts on the lease at the reviewed generation only: a
// generation the run no longer holds is refused, and a plan still drafting
// keeps its generation.
func TestRetryCheckpointLeasePinsReviewedGeneration(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	runKey := spekRepo + "!" + spekRunKey + ":" + StagePlan

	if retried, err := s.retryCheckpointLease(runKey, StagePlan, spekGen, now); err != nil || retried {
		t.Fatalf("retry of a drafting plan = %v, %v; want false, nil", retried, err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held plan advance: %v", err)
	}
	if _, err := s.retryCheckpointLease(runKey, StagePlan, spekGen+1, now); !errors.Is(err, errRunCheckpointNotHeld) {
		t.Fatalf("retry of a stale generation err = %v, want not held", err)
	}
	if stage, gen := spekLeaseState(hub); stage != StagePlan || gen != spekGen {
		t.Fatalf("lease after refused retry = %s gen %d, want plan gen %d", stage, gen, spekGen)
	}
	if retried, err := s.retryCheckpointLease(runKey, StagePlan, spekGen, now); err != nil || !retried {
		t.Fatalf("retry of the held plan = %v, %v; want true, nil", retried, err)
	}
	if stage, gen := spekLeaseState(hub); stage != StagePlan || gen <= spekGen {
		t.Fatalf("lease after retry = %s gen %d, want plan past gen %d", stage, gen, spekGen)
	}
}

// Rejecting a plan at its checkpoint supersedes the imported plan, so the plan
// the re-minted generation drafts replaces the rejected children even when its
// text is unchanged (hivecommons/hive#10063).
func TestPlanCheckpointRejectSupersedesImportedPlan(t *testing.T) {
	const taskList = "1. [T1] x [agent_suitable]"
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic := spekDraftEpic(t, store)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	rejected := runPlanChildren(store, epic.ID)
	if len(rejected) != 1 {
		t.Fatalf("imported children = %d, want 1", len(rejected))
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held plan advance: %v", err)
	}
	payload := planCheckpoint(t, s)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(payload.RunKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: payload.Gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject plan checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(epic.ID); got.Meta(runPlanDigestMeta) != runPlanDigestReplan {
		t.Fatalf("import digest after reject = %q, want %q", got.Meta(runPlanDigestMeta), runPlanDigestReplan)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("re-import after reject: %v", err)
	}
	if got, _ := store.Get(rejected[0].ID); got.Status != beads.StatusClosed {
		t.Fatalf("rejected child status after re-import = %q, want closed", got.Status)
	}
	if got := openRunPlanChildCount(store, epic.ID); got != 1 {
		t.Fatalf("open children after re-import = %d, want 1", got)
	}
}

// The plan page's reject runs the same hook, and supersedes the imported plan
// even when the run's lease is already gone (hivecommons/hive#10063).
func TestPlanRejectSupersedesImportedPlanWithoutLease(t *testing.T) {
	_, s, store, _ := spekHub(t)
	epic := spekDraftEpic(t, store)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}

	if err := s.resetRunLeaseAfterPlanReject(store, epic.ID); err != nil {
		t.Fatalf("resetRunLeaseAfterPlanReject: %v", err)
	}
	if got, _ := store.Get(epic.ID); got.Meta(runPlanDigestMeta) != runPlanDigestReplan {
		t.Fatalf("import digest after reject = %q, want %q", got.Meta(runPlanDigestMeta), runPlanDigestReplan)
	}
}

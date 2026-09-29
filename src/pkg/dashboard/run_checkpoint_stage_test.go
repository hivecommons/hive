package dashboard

import (
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
	_, s, store, epic := spekDesignRunAtPlanCheckpoint(t)
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

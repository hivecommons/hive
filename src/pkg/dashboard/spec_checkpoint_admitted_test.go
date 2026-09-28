package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/timeline"
)

// admittedSpecRun admits a triage-promoted spec run the way the scheduler,
// POST /api/runs/spec (non-design mode), nous and inception do: through
// AdmitTriagedRun, with no design epic and no GitHub client. It returns the
// server and the run's key as /api/runs exposes it.
func admittedSpecRun(t *testing.T, now time.Time) (*Server, string) {
	t.Helper()
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	deps.GHClient = nil
	disableSpekHubExecutorForRelayTests(s)
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = old })
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "feature label", now); err != nil {
		t.Fatalf("AdmitTriagedRun: %v", err)
	}
	runs, err := s.activeRuns(false)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %+v err=%v", runs, err)
	}
	return s, runs[0].Key
}

// finishAdmittedSpec writes the spec receipt through the same advance the
// runner uses; the default-blocking spec checkpoint parks the lease at spec.
func finishAdmittedSpec(t *testing.T, s *Server, runKey string, now time.Time) taskLease {
	t.Helper()
	held, ok := s.contributeHub.runLeaseHolder(runKey, now)
	if !ok {
		t.Fatal("admitted spec lease not found")
	}
	if err := s.AdvanceStageLease(held.identity, held.taskID, StagePlan, now, []byte(`{}`), nil); err != nil {
		t.Fatalf("AdvanceStageLease(spec->plan): %v", err)
	}
	after, ok := s.contributeHub.runLeaseHolder(runKey, now)
	if !ok || after.stage != StageSpec || after.gen != held.gen {
		t.Fatalf("spec lease after held advance = %+v ok=%v, want spec gen %d", after, ok, held.gen)
	}
	return after
}

func pendingStageNames(t *testing.T, s *Server) []string {
	t.Helper()
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	out := make([]string, 0, len(stages))
	for _, st := range stages {
		out = append(out, st.Stage)
	}
	return out
}

// TestAdmittedSpecRunWithoutDesignEpicIsApprovable is hivecommons/hive#9182:
// a triage-admitted spec run carries no design epic, so the default-blocking
// spec checkpoint held it forever - not offered, projected as waiting on an
// agent, and refused by the checkpoint API. The held stage must surface as
// waiting on a human and an owner approval must advance it to plan.
func TestAdmittedSpecRunWithoutDesignEpicIsApprovable(t *testing.T) {
	now := time.Now()
	s, runKey := admittedSpecRun(t, now)
	lease := finishAdmittedSpec(t, s, runKey, now)

	runs, err := s.activeRuns(false)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %+v err=%v", runs, err)
	}
	if runs[0].Stage != StageSpec || runs[0].WaitingOn != RunWaitingOnHuman || runs[0].WaitingReason != "checkpoint_enabled" {
		t.Fatalf("held epic-less spec run projection = stage %q waiting_on %q reason %q, want spec/human/checkpoint_enabled",
			runs[0].Stage, runs[0].WaitingOn, runs[0].WaitingReason)
	}
	if got := pendingStageNames(t, s); len(got) != 0 {
		t.Fatalf("held spec offered to relays: %v", got)
	}

	path := "/api/runs/" + url.PathEscape(runKey) + "/checkpoint"
	rec := doOwnerGet(s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload RunCheckpointPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	if payload.Stage != StageSpec || payload.Gen != lease.gen || payload.PlanEpicID != "" {
		t.Fatalf("checkpoint payload = %+v, want spec gen %d without epic", payload, lease.gen)
	}

	rec = doOwnerPost(s, path, runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: lease.gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve epic-less spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	advanced, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok || advanced.stage != StagePlan {
		t.Fatalf("lease after spec approval = %+v ok=%v, want plan", advanced, ok)
	}
	if got := pendingStageNames(t, s); len(got) != 1 || got[0] != StagePlan {
		t.Fatalf("pending stages after approval = %v, want [plan]", got)
	}
	approved := false
	for _, ev := range s.LifecycleTimeline().ByIssue(runKey) {
		if ev.Kind == timeline.KindStageApproval && ev.Attrs[stageAttrStage] == StageSpec && ev.Attrs[runCheckpointActorKey] != "" {
			approved = true
		}
	}
	if !approved {
		t.Fatal("spec approval left no stage_approval timeline event")
	}
}

// TestAdmittedSpecRunRejectReopensSpec pins the reject half: rejecting an
// epic-less spec checkpoint re-mints the spec stage so it is offered again
// instead of staying parked behind the old receipt.
func TestAdmittedSpecRunRejectReopensSpec(t *testing.T) {
	now := time.Now()
	s, runKey := admittedSpecRun(t, now)
	lease := finishAdmittedSpec(t, s, runKey, now)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: lease.gen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject epic-less spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	retried, ok := s.contributeHub.runLeaseHolder(runKey, time.Now())
	if !ok || retried.stage != StageSpec || retried.gen <= lease.gen {
		t.Fatalf("lease after spec reject = %+v ok=%v, want spec above gen %d", retried, ok, lease.gen)
	}
	if got := pendingStageNames(t, s); len(got) != 1 || got[0] != StageSpec {
		t.Fatalf("pending stages after reject = %v, want [spec]", got)
	}
	runs, err := s.activeRuns(false)
	if err != nil || len(runs) != 1 || runs[0].WaitingOn != RunWaitingOnAgent {
		t.Fatalf("run after reject = %+v err=%v, want waiting on agent", runs, err)
	}
}

// TestAdmittedSpecRunCheckpointRefusedBeforeReceipt keeps the checkpoint
// closed while the spec is still being written: with no receipt there is
// nothing to approve, and approving would skip the spec entirely.
func TestAdmittedSpecRunCheckpointRefusedBeforeReceipt(t *testing.T) {
	now := time.Now()
	s, runKey := admittedSpecRun(t, now)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: 1})
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve unfinished spec = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if held, ok := s.contributeHub.runLeaseHolder(runKey, now); !ok || held.stage != StageSpec {
		t.Fatalf("unfinished spec lease = %+v ok=%v, want spec", held, ok)
	}
}

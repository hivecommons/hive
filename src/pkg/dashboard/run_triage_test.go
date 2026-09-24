package dashboard

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAdmitRunIdempotentAndRequiresSpektacular(t *testing.T) {
	s, deps := runsTestServer(t)
	now := time.Now()
	if err := s.AdmitRun("myorg/repo1", 8450, "feature", now); err == nil {
		t.Fatal("AdmitRun succeeded with spektacular disabled")
	}
	deps.Config.Runs.Spektacular.Enabled = true
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "feature label", now); err != nil {
		t.Fatalf("AdmitRun: %v", err)
	}
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "feature label", now.Add(time.Minute)); err != nil {
		t.Fatalf("AdmitRun idempotent: %v", err)
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Stage != StageSpec || stages[0].Repo != "myorg/repo1" {
		t.Fatalf("pending stages = %+v", stages)
	}
	held, ok := s.contributeHub.runLeaseHolder("myorg/repo1!myorg/repo1#8450:spec", now)
	if !ok {
		t.Fatal("admitted lease not found")
	}
	if _, err := s.contributeHub.mutateLeaseStage(held.identity, held.taskID, StagePlan, leaseStageAdvance, "", now, time.Time{}); err != nil {
		t.Fatalf("advance admitted lease: %v", err)
	}
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "feature label", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("AdmitRun after advance: %v", err)
	}
	if runs, err := s.activeRuns(false); err != nil || len(runs) != 1 || runs[0].Stage != StagePlan {
		t.Fatalf("advanced run idempotency broke: runs=%+v err=%v", runs, err)
	}
	runs, err := s.activeRuns(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].TriageVerdict != "spec" || runs[0].TriageRationale != "feature label" || !strings.Contains(runs[0].Title, "feature") {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestRunResetTriageFixRetiresSpecRun(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	if err := s.AdmitTriagedRun("myorg/repo1", 8450, "feature", "spec", "feature label", time.Now()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.activeRuns(false)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %+v err=%v", runs, err)
	}
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runs[0].Key)+"/reset", map[string]string{"reason": runResetReasonTriageFix})
	if rec.Code != 200 {
		t.Fatalf("reset = %d body=%s", rec.Code, rec.Body.String())
	}
	if !s.RunTriageFixRetired("myorg/repo1", 8450) {
		t.Fatal("triage_fix reset did not record direct-fix suppression")
	}
	runs, err = s.activeRuns(false)
	if err != nil || len(runs) != 0 {
		t.Fatalf("after reset activeRuns = %+v err=%v", runs, err)
	}
}

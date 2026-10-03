package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

func TestOwnerResetRestartsOnlyEscalatedStage(t *testing.T) {
	for _, stage := range []string{StageSpec, StagePlan, StageImplement} {
		t.Run(stage, func(t *testing.T) {
			s, _ := runsTestServer(t)
			now := time.Now()
			recordRunResetLease(t, s, stage, now)
			outcome, _, _, err := s.contributeHub.settleStageGeneration(runResetTestIdentity, runResetTestTask, runResetTestGen, 1, now)
			if err != nil || outcome != stageSettleEscalated {
				t.Fatalf("settle = %v, %v", outcome, err)
			}
			rec := doOwnerPost(s, runResetTestPath, runResetRequest{To: stage, Reason: "owner resolved escalation"})
			if rec.Code != http.StatusOK {
				t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
			}
			var resp runResetResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			lease := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, resp.Gen, now)
			if lease == nil || lease.stage != stage || lease.stageRetries != 0 || !lease.stageEscalatedAt.IsZero() || resp.Gen <= runResetTestGen {
				t.Fatalf("fresh lease = %+v", lease)
			}
			if old := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now); old != nil {
				t.Fatal("old generation accepted")
			}
			// Once reset, the same-stage operation is again forbidden.
			rec = doOwnerPost(s, runResetTestPath, runResetRequest{To: stage, Reason: "again"})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("ordinary same-stage reset = %d", rec.Code)
			}
		})
	}
}

func TestRunAbandonPersistsAndSuppressesAdmission(t *testing.T) {
	s, deps := runsTestServer(t)
	deps.Config.Runs.Spektacular.Enabled = true
	h := s.contributeHub
	h.persistTaskLedgers = true
	h.taskLeasesFile = filepath.Join(t.TempDir(), "leases.json")
	now := time.Now()
	recordRunResetLease(t, s, StageSpec, now)
	if _, _, _, err := h.settleStageGeneration(runResetTestIdentity, runResetTestTask, runResetTestGen, 1, now); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSuffix(runResetTestPath, "reset") + "abandon"
	rec := doOwnerPost(s, path, runAbandonRequest{Reason: "no longer needed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("abandon = %d: %s", rec.Code, rec.Body.String())
	}
	if h.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now) != nil {
		t.Fatal("abandoned lease re-adopted")
	}
	if !s.RunAbandoned(runResetTestKey) {
		t.Fatal("missing retirement")
	}
	var run Run
	detail := doGet(s, runResetTestDetail)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail = %d: %s", detail.Code, detail.Body.String())
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.State != runRetiredAbandoned {
		t.Fatalf("run = %+v", run)
	}
	// Restore from the lease ledger, not the timeline or live hub memory.
	h.retiredRuns = nil
	h.loadLeases()
	if !s.RunAbandoned(runResetTestKey) {
		t.Fatal("abandonment lost on restart")
	}
	if err := s.AdmitRun("myorg/repo1", 8350, "feature", now); err == nil {
		t.Fatal("abandoned run re-admitted")
	}
	// The explicit owner design path shares admission's terminal run-key
	// contract too; it cannot clear the persisted tombstone.
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.startDesignSpektacular(context.Background(), store, github.Issue{Repo: "myorg/repo1", Number: 8350, Title: "Abandoned design"}, "", false); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("explicit design start = %v", err)
	}
	if rec := doOwnerPost(s, runResetTestPath, runResetRequest{To: StageSpec, Reason: "restart"}); rec.Code != http.StatusNotFound {
		t.Fatalf("abandoned reset = %d: %s", rec.Code, rec.Body.String())
	}
	if !s.RunAbandoned(runResetTestKey) {
		t.Fatal("owner start or reset reversed abandonment")
	}
	if err := h.recordLeaseForKeyStage("bob", "fresh", "myorg/repo1", 8350, runResetTestKey, "contributor", StageSpec, 20, now); err == nil {
		t.Fatal("abandoned run assigned")
	}
	epic, key, err := s.StartDesignSpektacularFromIssue(context.Background(), nil, github.Issue{Repo: "myorg/repo1", Number: 8350, Labels: []string{"design"}})
	if err != nil || epic != nil || key != "" {
		t.Fatalf("automatic design restarted: %v %q %v", epic, key, err)
	}
}

func TestRunAbandonGuardsAndRollback(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	recordRunResetLease(t, s, StagePlan, now)
	path := strings.TrimSuffix(runResetTestPath, "reset") + "abandon"
	for _, role := range []string{"", "read-write", "owner"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"stop"}`))
		req.Header.Set("X-Hive-Role", role)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("proofless %q = %d", role, rec.Code)
		}
	}
	if rec := doOwnerPost(s, path, runAbandonRequest{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason = %d", rec.Code)
	}
	if rec := doOwnerPost(s, "/api/runs/missing/abandon", runAbandonRequest{Reason: "stop"}); rec.Code != http.StatusNotFound {
		t.Fatalf("missing run = %d", rec.Code)
	}
	h := s.contributeHub
	held, _ := h.runLeaseHolder(runResetTestKey, now)
	stale := held
	stale.gen++
	if err := h.retireRun(stale, runRetiredAbandoned); err != errLeaseGenStale {
		t.Fatalf("stale retire = %v", err)
	}
	// A file used as a parent directory deterministically fails persistence,
	// even when CI runs as root.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	h.persistTaskLedgers = true
	h.taskLeasesFile = filepath.Join(blocker, "leases.json")
	if rec := doOwnerPost(s, path, runAbandonRequest{Reason: "stop"}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("write failure = %d: %s", rec.Code, rec.Body.String())
	}
	if s.RunAbandoned(runResetTestKey) {
		t.Fatal("failed write retired the run")
	}
	if lease := h.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now); lease == nil || lease.stage != StagePlan {
		t.Fatalf("failed write lost lease: %+v", lease)
	}
}

func TestTriageFixSuppressesAutomaticDesignAfterRestart(t *testing.T) {
	s, _ := runsTestServer(t)
	h := s.contributeHub
	h.persistTaskLedgers = true
	h.taskLeasesFile = filepath.Join(t.TempDir(), "leases.json")
	recordRunResetLease(t, s, StageSpec, time.Now())
	if rec := doOwnerPost(s, runResetTestPath, runResetRequest{Reason: runResetReasonTriageFix}); rec.Code != http.StatusOK {
		t.Fatalf("triage_fix = %d: %s", rec.Code, rec.Body.String())
	}
	resetLifecycleStore()
	h.retiredRuns = nil
	h.loadLeases()
	if !s.RunTriageFixRetired("myorg/repo1", 8350) {
		t.Fatal("triage retirement lost on restart")
	}
	epic, key, err := s.StartDesignSpektacularFromIssue(context.Background(), nil, github.Issue{Repo: "myorg/repo1", Number: 8350, Labels: []string{"design"}})
	if err != nil || epic != nil || key != "" {
		t.Fatalf("retired design restarted: %v %q %v", epic, key, err)
	}
}

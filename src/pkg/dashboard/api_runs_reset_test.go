package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// api_runs_reset_test.go covers POST /api/runs/{key}/reset (#8350): the
// owner-only route that moves a run back to an earlier stage.

const (
	runResetTestKey      = "myorg/repo1#8350"
	runResetTestPath     = "/api/runs/myorg%2Frepo1%238350/reset"
	runResetTestDetail   = "/api/runs/myorg%2Frepo1%238350"
	runResetTestIdentity = "alice"
	runResetTestTask     = "task-8350"
	runResetTestGen      = uint64(12)
)

func recordRunResetLease(t *testing.T, s *Server, stage string, now time.Time) {
	t.Helper()
	if err := s.contributeHub.recordLeaseForKeyStage(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350,
		runResetTestKey, "contributor", stage, runResetTestGen, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
}

func TestRunResetMovesBackAndShowsReasonInHistory(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	recordRunResetLease(t, s, StageImplement, now)

	rec := doOwnerPost(s, runResetTestPath, runResetRequest{To: StagePlan, Reason: "plan rejected"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST reset = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp runResetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reset response: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK || resp.Key != runResetTestKey || resp.StageFrom != StageImplement ||
		resp.Stage != StagePlan || resp.Gen <= runResetTestGen || resp.Reason != "plan rejected" {
		t.Fatalf("unexpected reset response: %+v", resp)
	}
	if stale := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now); stale != nil {
		t.Fatalf("old generation still re-adopts after reset: %+v", stale)
	}

	detail := doGet(s, runResetTestDetail)
	if detail.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", detail.Code, detail.Body.String())
	}
	var run Run
	if err := json.Unmarshal(detail.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Stage != StagePlan || run.Gen != resp.Gen {
		t.Fatalf("run after reset = stage %q gen %d, want %q/%d", run.Stage, run.Gen, StagePlan, resp.Gen)
	}
	foundReason := false
	for _, stage := range run.Stages {
		if stage.Status == "observed" && stage.Gen == resp.Gen && stage.Reason == "plan rejected" {
			foundReason = true
		}
	}
	if !foundReason {
		t.Fatalf("run history does not show the reset reason: %+v", run.Stages)
	}

	audited := false
	for _, e := range s.audit.Recent(0) {
		if e.Action == auditActionRunStageReset && strings.Contains(e.Detail, "stage_from="+StageImplement) &&
			strings.Contains(e.Detail, "stage_to="+StagePlan) && strings.Contains(e.Detail, "reason=plan rejected") {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("owner reset was not audited against the request: %+v", s.audit.Recent(0))
	}
}

func TestRunResetRefusesBadRequests(t *testing.T) {
	tests := []struct {
		name     string
		stage    string
		path     string
		body     runResetRequest
		wantCode int
	}{
		{"forward plan to implement", StagePlan, runResetTestPath, runResetRequest{To: StageImplement, Reason: "x"}, http.StatusBadRequest},
		{"same stage", StagePlan, runResetTestPath, runResetRequest{To: StagePlan, Reason: "x"}, http.StatusBadRequest},
		{"unknown stage", StageImplement, runResetTestPath, runResetRequest{To: "deploy", Reason: "x"}, http.StatusBadRequest},
		{"missing reason", StageImplement, runResetTestPath, runResetRequest{To: StagePlan}, http.StatusBadRequest},
		{"missing target", StageImplement, runResetTestPath, runResetRequest{Reason: "x"}, http.StatusBadRequest},
		{"unknown run", StageImplement, "/api/runs/myorg%2Frepo1%239999/reset", runResetRequest{To: StagePlan, Reason: "x"}, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := runsTestServer(t)
			now := time.Now()
			recordRunResetLease(t, s, tc.stage, now)

			rec := doOwnerPost(s, tc.path, tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("POST reset = %d body=%s, want %d", rec.Code, rec.Body.String(), tc.wantCode)
			}
			cur := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now)
			if cur == nil || cur.stage != tc.stage {
				t.Fatalf("refused reset changed the lease: %+v", cur)
			}
		})
	}
}

func TestRunResetRejectsNonOwner(t *testing.T) {
	tests := []struct {
		name string
		mark func(*http.Request)
	}{
		{"read-write member", func(req *http.Request) { req.Header.Set("X-Hive-Role", "read-write") }},
		{"proofless owner", func(req *http.Request) { req.Header.Set("X-Hive-Role", "owner") }},
		{"no role", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := runsTestServer(t)
			now := time.Now()
			recordRunResetLease(t, s, StageImplement, now)

			body := strings.NewReader(`{"to":"plan","reason":"plan rejected"}`)
			req := httptest.NewRequest(http.MethodPost, runResetTestPath, body)
			req.Header.Set("Content-Type", "application/json")
			if tc.mark != nil {
				tc.mark(req)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("non-owner POST reset = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusForbidden)
			}
			cur := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now)
			if cur == nil || cur.stage != StageImplement {
				t.Fatalf("refused non-owner reset changed the lease: %+v", cur)
			}
		})
	}
}

func TestRunResetPersistFailureSurfaces(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory modes do not stop root; cannot inject a write failure")
	}
	s, _ := runsTestServer(t)
	dir := filepath.Join(t.TempDir(), "ws-state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("building leases directory: %v", err)
	}
	s.contributeHub.persistTaskLedgers = true
	s.contributeHub.taskLeasesFile = filepath.Join(dir, "task-leases.json")
	now := time.Now()
	recordRunResetLease(t, s, StageImplement, now)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making leases directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	rec := doOwnerPost(s, runResetTestPath, runResetRequest{To: StagePlan, Reason: "plan rejected"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST reset on unwritable registry = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusInternalServerError)
	}
	cur := s.contributeHub.lookupLease(runResetTestIdentity, runResetTestTask, "myorg/repo1", 8350, runResetTestGen, now)
	if cur == nil || cur.stage != StageImplement {
		t.Fatalf("failed reset did not roll the lease back: %+v", cur)
	}
}

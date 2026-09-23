package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

func runsTestServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	s.contributeHub.persistTaskLedgers = false
	return s, deps
}

func runsGet(s *Server, path string, mark func(*http.Request)) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if mark != nil {
		mark(req)
	}
	s.authenticate(s.roleEnforcement(s.mux)).ServeHTTP(rec, req)
	return rec
}

func decodeRuns(t *testing.T, rec *httptest.ResponseRecorder) []Run {
	t.Helper()
	var runs []Run
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode runs: %v body=%s", err, rec.Body.String())
	}
	return runs
}

func TestRunsListNoRunLeasesAndStatusZeros(t *testing.T) {
	s, _ := runsTestServer(t)

	rec := runsGet(s, "/api/runs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs = %d body=%s", rec.Code, rec.Body.String())
	}
	if runs := decodeRuns(t, rec); len(runs) != 0 {
		t.Fatalf("runs = %+v, want empty list", runs)
	}

	status := &StatusPayload{}
	s.UpdateStatus(status)
	if status.Runs.Active != 0 || status.Runs.WaitingOnHuman != 0 {
		t.Fatalf("status runs = %+v, want zeros", status.Runs)
	}
}

func TestRunsPlanDraftWaitsOnHuman(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("draft plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8299"
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	updated, _ := store.Get(epic.ID)
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	now := time.Now()
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8299", "myorg/repo1", 8299, "myorg/repo1#8299", "contributor", StagePlan, 9, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := runsGet(s, "/api/runs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs = %d body=%s", rec.Code, rec.Body.String())
	}
	runs := decodeRuns(t, rec)
	if len(runs) != 1 {
		t.Fatalf("runs len = %d, want 1: %+v", len(runs), runs)
	}
	run := runs[0]
	if run.WaitingOn != RunWaitingOnHuman {
		t.Fatalf("waiting_on = %q, want human: %+v", run.WaitingOn, run)
	}
	if run.WaitingSince != formatRunTime(updated.UpdatedAt.Time) {
		t.Fatalf("waiting_since = %q, want %q", run.WaitingSince, formatRunTime(updated.UpdatedAt.Time))
	}
	if run.PlanEpicID != epic.ID {
		t.Fatalf("plan_epic_id = %q, want %q", run.PlanEpicID, epic.ID)
	}
}

func TestRunsAuthBoundaryAndNonOwnerRead(t *testing.T) {
	s, _ := runsTestServer(t)
	s.authToken = "secret"

	if rec := runsGet(s, "/api/runs", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/runs = %d, want 401", rec.Code)
	}
	rec := runsGet(s, "/api/runs", func(req *http.Request) {
		req.Header.Set("X-Hive-User", "reader")
		req.Header.Set("X-Hive-Role", config.RoleRead)
		req.Header.Set(proxyAuthHeader, s.authToken)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only GET /api/runs = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunProjectionScrubsTitleAndDetailUsesTimeline(t *testing.T) {
	s, _ := runsTestServer(t)
	now := time.Now()
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-secret", "myorg/repo1", 8299, "myorg/repo1#8299", "contributor", StageImplement, 12, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.contributeHub.mu.Lock()
	s.contributeHub.connections["conn"] = &ContributorConnection{
		profile:        &ContributorProfile{ContributorID: "alice"},
		currentTask:    &WSTaskAssign{TaskID: "task-secret", Repo: "myorg/repo1", Number: 8299, Title: "fix token ghp_abcdefghijklmnopqrstuvwxyz1234567890"},
		taskAssignedAt: now,
	}
	s.contributeHub.mu.Unlock()
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: "myorg/repo1#8299",
		Kind:     timeline.Kind("plan"),
		At:       now.Add(time.Minute).UnixMilli(),
		Attrs:    map[string]string{"gen": "12", "receipt_digest": "sha256:abc"},
	})

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238299")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Title == "fix token ghp_abcdefghijklmnopqrstuvwxyz1234567890" {
		t.Fatal("run title was not scrubbed")
	}
	foundReceipt := false
	for _, stage := range run.Stages {
		if stage.Name == "plan" && stage.Receipt == "sha256:abc" {
			foundReceipt = true
		}
	}
	if !foundReceipt {
		t.Fatalf("detail stages did not include timeline receipt: %+v", run.Stages)
	}
}

func TestRunsRegistryUnavailableErrors(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	s.deps = testDeps(t)
	s.mux.HandleFunc("GET /api/runs", s.handleRunsList)

	rec := doGet(s, "/api/runs")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/runs with nil registry = %d, want 503", rec.Code)
	}
}

func TestRunsHumanReviewHoldWaitsOnHuman(t *testing.T) {
	s, _ := runsTestServer(t)
	dir := t.TempDir()
	oldPath := runReviewDispatchStatePath
	runReviewDispatchStatePath = filepath.Join(dir, "review-dispatch-state.json")
	t.Cleanup(func() {
		runReviewDispatchStatePath = oldPath
	})
	holdAt := time.Now().Add(-time.Minute).UTC()
	data, err := json.Marshal(map[string]any{
		"requires_human": []runHumanReviewHold{{Repo: "myorg/repo1", Number: 8299, UpdatedAt: holdAt}},
	})
	if err != nil {
		t.Fatalf("marshal review state: %v", err)
	}
	if err := os.WriteFile(runReviewDispatchStatePath, data, 0o600); err != nil {
		t.Fatalf("write review state: %v", err)
	}
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-human", "myorg/repo1", 8299, "myorg/repo1#8299", "contributor", StageImplement, 13, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := doGet(s, "/api/runs")
	runs := decodeRuns(t, rec)
	if len(runs) != 1 || runs[0].WaitingOn != RunWaitingOnHuman || runs[0].WaitingSince != formatRunTime(holdAt) {
		t.Fatalf("human hold run = %+v", runs)
	}
}

func TestRunDetailGroupsWavePRsUnderSingleReviewAction(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("multi repo run", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8314"
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	res, err := planning.DecomposeFromOutput(store, epic, `1. [T1] API work [repo:myorg/api] [role:service] [wave:1] [agent_suitable]
2. [T2] UI work [repo:myorg/ui] [role:consumer] [wave:1] [agent_suitable]
`, planning.Options{})
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	for i, child := range res.Children {
		pr := "https://github.com/myorg/" + []string{"api", "ui"}[i] + "/pull/10"
		if err := store.SetMetadata(child.ID, planning.MetaPRURL, pr); err != nil {
			t.Fatalf("set pr url: %v", err)
		}
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8314", "myorg/repo1", 8314, "myorg/repo1#8314", "contributor", StageImplement, 13, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238314")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if len(run.ReviewWaves) != 1 || run.ReviewWaves[0].Wave != 1 || run.ReviewWaves[0].ApproveAction != "approve_plan_wave" {
		t.Fatalf("review waves = %+v", run.ReviewWaves)
	}
	if len(run.ReviewWaves[0].PRs) != 2 {
		t.Fatalf("wave PRs = %+v", run.ReviewWaves[0].PRs)
	}
}

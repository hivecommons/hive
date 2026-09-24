package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

func runsTestServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	resetLifecycleStore()
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

func TestRunDetailSurfacesStageApprovalTimelineEvent(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("approved run plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8582"
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	const (
		runKey = "myorg/repo1#8582"
		actor  = "dana"
		gen    = uint64(9)
	)
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8582", "myorg/repo1", 8582, runKey, "contributor", StageImplement, 10, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	base := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC)
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: runKey,
		Kind:     timeline.KindStageReceipt,
		Agent:    "alice",
		At:       base.UnixMilli(),
		Attrs: map[string]string{
			stageAttrStage: StagePlan,
			stageAttrGen:   strconv.FormatUint(gen, 10),
			"receipt":      "sha256:plan",
		},
	})
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: runKey,
		Kind:     timeline.KindStageApproval,
		Agent:    actor,
		At:       base.Add(time.Minute).UnixMilli(),
		Attrs: map[string]string{
			stageAttrRunKey:       runKey,
			stageAttrStage:        StagePlan,
			stageAttrGen:          strconv.FormatUint(gen, 10),
			runCheckpointEpicKey:  epic.ID,
			runCheckpointActorKey: actor,
		},
	})

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238582")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	var approval *RunStage
	for i := range run.Stages {
		if run.Stages[i].Name == string(timeline.KindStageApproval) {
			approval = &run.Stages[i]
		}
	}
	if approval == nil {
		t.Fatalf("stage approval not surfaced: %+v", run.Stages)
	}
	if approval.Status != "observed" || approval.Gen != gen || approval.Actor != actor || approval.At != formatRunTime(base.Add(time.Minute)) {
		t.Fatalf("approval projection = %+v", approval)
	}
	if approval.Attrs[stageAttrRunKey] != runKey ||
		approval.Attrs[stageAttrStage] != StagePlan ||
		approval.Attrs[runCheckpointEpicKey] != epic.ID ||
		approval.Attrs[runCheckpointActorKey] != actor {
		t.Fatalf("approval attrs = %+v", approval.Attrs)
	}
}

func TestRunImplementTaskCompleteEndsRunAndFiresHook(t *testing.T) {
	s, deps := runsTestServer(t)
	capture := &hookCapture{}
	deps.HookFire = capture.fire
	now := time.Now().Add(-time.Minute)
	const (
		identity = "alice"
		taskID   = "task-implement"
		repo     = "myorg/repo1"
		number   = 8460
		key      = "myorg/repo1#8460"
		gen      = uint64(7)
	)
	if err := s.contributeHub.recordLeaseForKeyStage(identity, taskID, repo, number, key, "contributor", StageImplement, gen, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	conn := &ContributorConnection{
		profile:        &ContributorProfile{ContributorID: identity, GitHubUsername: identity},
		currentTask:    &WSTaskAssign{TaskID: taskID, Kind: "run", Stage: StageImplement, Repo: repo, Number: number, Key: key, Title: "implement gap 7"},
		currentTaskGen: gen,
		taskAssignedAt: now,
	}
	session := &wsSession{h: s.contributeHub, contributor: conn}

	session.handleTaskComplete(WSMessage{Type: "task_complete", TaskID: taskID, TaskGen: gen, Result: "completed"})

	runs, err := s.activeRuns(true)
	if err != nil {
		t.Fatalf("activeRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs len = %d, want completed run: %+v", len(runs), runs)
	}
	run := runs[0]
	if run.Key != key || run.State != "completed" || run.Stage != "completed" || run.CompletedAt == "" {
		t.Fatalf("completed run projection = %+v", run)
	}
	if run.WaitingOn != RunWaitingOnNone {
		t.Fatalf("waiting_on = %q, want none", run.WaitingOn)
	}
	if stages, err := s.RunStageAccessor().PendingRunStages(context.Background()); err != nil || len(stages) != 0 {
		t.Fatalf("pending run stages after completion = %+v, %v", stages, err)
	}
	hooks := capture.all()
	if len(hooks) != 1 {
		t.Fatalf("hooks len = %d, want 1: %+v", len(hooks), hooks)
	}
	if hooks[0].StageFrom != StageImplement || hooks[0].StageTo != "completed" || hooks[0].Gen != gen {
		t.Fatalf("stage_completed hook = %+v", hooks[0])
	}
}

func TestRunImplementTaskFailedDoesNotCompleteRun(t *testing.T) {
	s, deps := runsTestServer(t)
	capture := &hookCapture{}
	deps.HookFire = capture.fire
	now := time.Now().Add(-time.Minute)
	const (
		identity = "alice"
		taskID   = "task-implement-failed"
		repo     = "myorg/repo1"
		number   = 8461
		key      = "myorg/repo1#8461"
		gen      = uint64(7)
	)
	if err := s.contributeHub.recordLeaseForKeyStage(identity, taskID, repo, number, key, "contributor", StageImplement, gen, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	conn := &ContributorConnection{
		profile:        &ContributorProfile{ContributorID: identity, GitHubUsername: identity},
		currentTask:    &WSTaskAssign{TaskID: taskID, Kind: "run", Stage: StageImplement, Repo: repo, Number: number, Key: key, Title: "implement gap 7"},
		currentTaskGen: gen,
		taskAssignedAt: now,
	}
	session := &wsSession{h: s.contributeHub, contributor: conn}

	session.handleTaskFailed(WSMessage{Type: "task_failed", TaskID: taskID, TaskGen: gen, Reason: "tests failed"})

	runs, err := s.activeRuns(true)
	if err != nil {
		t.Fatalf("activeRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("failed implement stage should not complete a run: %+v", runs)
	}
	if hooks := capture.all(); len(hooks) != 0 {
		t.Fatalf("failure fired stage_completed hooks: %+v", hooks)
	}
}

func TestRunDetailIncludesBurndownWhenSourceMatches(t *testing.T) {
	s, deps := runsTestServer(t)
	key := "myorg/repo1#8299"
	leaseKey := "myorg/repo1!myorg/repo1#8299:implement"
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-audit", "myorg/repo1", 8299, leaseKey, "contributor", StageImplement, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	deps.RunBurndown = func(_ context.Context, got string) (*RunBurndown, error) {
		if got != key {
			t.Fatalf("burndown key = %q, want %q", got, key)
		}
		return &RunBurndown{Source: "audit", Satisfied: 7, Remaining: 2, Unknown: 1, Scope: 10}, nil
	}

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238299")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Burndown == nil || run.Burndown.Source != "audit" || run.Burndown.Satisfied != 7 ||
		run.Burndown.Remaining != 2 || run.Burndown.Unknown != 1 || run.Burndown.Scope != 10 {
		t.Fatalf("burndown = %+v", run.Burndown)
	}
	if run.Key != key || run.LeaseKey != leaseKey || run.Repo != "myorg/repo1" {
		t.Fatalf("run keys = key %q lease %q repo %q", run.Key, run.LeaseKey, run.Repo)
	}
}

func TestWavefrontQueueItemHasRunBurndownDetail(t *testing.T) {
	s, deps := runsTestServer(t)
	const (
		repo = "clubanderson/hive-runs-e2e"
		key  = "clubanderson/hive-runs-e2e!crustify-fixture:parse-ast"
	)
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "hive-runs-e2e",
		Full: repo,
		ActionableIssues: []any{map[string]any{
			"source_type": "run",
			"external_id": "crustify-fixture:parse-ast",
			"title":       "implement: Port the AST parser to Rust",
			"labels":      []string{"hive-run", "stage/implement", "wavefront", "graph-rev/rev-7"},
			"state":       "open",
		}},
	}}}
	deps.RunBurndown = func(_ context.Context, got string) (*RunBurndown, error) {
		if got != key {
			t.Fatalf("burndown key = %q, want %q", got, key)
		}
		return &RunBurndown{Source: "wavefront", Satisfied: 2, Remaining: 22, Unknown: 0, Scope: 24}, nil
	}

	queueRec := doGet(s, "/api/contribute/queue")
	if queueRec.Code != http.StatusOK {
		t.Fatalf("GET queue = %d body=%s", queueRec.Code, queueRec.Body.String())
	}
	var queue struct {
		Queue []ReadyQueueItem `json:"queue"`
	}
	if err := json.Unmarshal(queueRec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(queue.Queue) != 1 || queue.Queue[0].Key != key || queue.Queue[0].SourceType != "run" ||
		!contains(queue.Queue[0].Labels, "wavefront") {
		t.Fatalf("queue = %+v", queue.Queue)
	}

	detailRec := doGet(s, "/api/runs/"+url.PathEscape(key))
	if detailRec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var run Run
	if err := json.Unmarshal(detailRec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Key != key || run.Repo != repo || run.Stage != StageImplement || run.State != "queued" {
		t.Fatalf("run projection = %+v", run)
	}
	if run.Burndown == nil || run.Burndown.Source != "wavefront" || run.Burndown.Scope != 24 ||
		run.Burndown.Satisfied != 2 || run.Burndown.Remaining != 22 {
		t.Fatalf("burndown = %+v", run.Burndown)
	}
}

func TestQueuedRunDetailLookupIsUnboundedAndSkipsHeld(t *testing.T) {
	s, deps := runsTestServer(t)
	const repo = "clubanderson/hive-runs-e2e"
	items := make([]any, 0, readyQueueDefaultLimit+1)
	for i := 0; i < readyQueueDefaultLimit+1; i++ {
		items = append(items, map[string]any{
			"source_type": "run",
			"external_id": "crustify-fixture:node-" + strconv.Itoa(i),
			"title":       "implement: node",
			"labels":      []string{"hive-run", "stage/implement", "wavefront"},
			"state":       "open",
		})
	}
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name:             "hive-runs-e2e",
		Full:             repo,
		ActionableIssues: items,
	}}}
	key := repo + "!crustify-fixture:node-" + strconv.Itoa(readyQueueDefaultLimit)
	deps.RunBurndown = func(context.Context, string) (*RunBurndown, error) { return nil, nil }
	if rec := doGet(s, "/api/runs/"+url.PathEscape(key)); rec.Code != http.StatusOK {
		t.Fatalf("detail for item beyond display limit = %d body=%s", rec.Code, rec.Body.String())
	}

	deps.Config.Hub.ContributeQueueHold = []string{key}
	if rec := doGet(s, "/api/runs/"+url.PathEscape(key)); rec.Code != http.StatusNotFound {
		t.Fatalf("held queued detail = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunsCanonicalKeyListAndDetailAcceptsLeaseKey(t *testing.T) {
	s, _ := runsTestServer(t)
	leaseKey := "repo1!repo1#8533:spec"
	if err := s.contributeHub.recordLeaseForKeyStage("hive-triage", "task-8533", "repo1", 8533, leaseKey, "triage", StageSpec, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := doGet(s, "/api/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET runs = %d body=%s", rec.Code, rec.Body.String())
	}
	runs := decodeRuns(t, rec)
	if len(runs) != 1 {
		t.Fatalf("runs len = %d: %+v", len(runs), runs)
	}
	if runs[0].Key != "myorg/repo1#8533" || runs[0].LeaseKey != leaseKey || runs[0].Repo != "myorg/repo1" {
		t.Fatalf("run projection = %+v", runs[0])
	}
	for _, path := range []string{"/api/runs/myorg%2Frepo1%238533", "/api/runs/" + url.PathEscape(leaseKey)} {
		rec := doGet(s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRunBurndownOmittedWithoutSourceAndFromList(t *testing.T) {
	s, deps := runsTestServer(t)
	key := "myorg/repo1#8299"
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-8299", "myorg/repo1", 8299, key, "contributor", StageImplement, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	called := false
	deps.RunBurndown = func(context.Context, string) (*RunBurndown, error) {
		called = true
		return nil, nil
	}

	listRec := doGet(s, "/api/runs")
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET runs = %d body=%s", listRec.Code, listRec.Body.String())
	}
	if called {
		t.Fatal("list handler called burndown source")
	}

	detailRec := doGet(s, "/api/runs/myorg%2Frepo1%238299")
	if detailRec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(detailRec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if _, ok := raw["burndown"]; ok {
		t.Fatalf("burndown should be omitted: %s", detailRec.Body.String())
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

func TestRunsReportsStalePlanReason(t *testing.T) {
	s, deps := runsTestServer(t)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("stale plan", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusApproved
		b.Metadata[planning.MetaIssueRepo] = "myorg/repo1"
		b.Metadata[planning.MetaIssueNumber] = "8346"
		b.Metadata[planning.MetaRunWaitingOn] = "human"
		b.Metadata[planning.MetaRunWaitingReason] = planning.WaitingReasonStalePlan
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	deps.BeadStores = map[string]*beads.Store{"architect": store}
	if err := s.contributeHub.recordLeaseForKeyStage("alice", "task-stale", "myorg/repo1", 8346, "myorg/repo1#8346", "contributor", StageImplement, 13, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}

	rec := doGet(s, "/api/runs")
	runs := decodeRuns(t, rec)
	if len(runs) != 1 || runs[0].WaitingOn != RunWaitingOnHuman || runs[0].WaitingReason != planning.WaitingReasonStalePlan {
		t.Fatalf("stale plan run = %+v", runs)
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

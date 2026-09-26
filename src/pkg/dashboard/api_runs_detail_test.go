package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/timeline"
)

func TestRunDetailAggregatesIssueReceiptsPRsAndTranscripts(t *testing.T) {
	s, _ := runsTestServer(t)
	oldReceipts, oldRunLog := runReceiptsDir, taskRunLogPath
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	taskRunLogPath = filepath.Join(t.TempDir(), "task-runs.jsonl")
	t.Cleanup(func() {
		runReceiptsDir = oldReceipts
		taskRunLogPath = oldRunLog
	})

	const (
		repo     = "myorg/repo1"
		key      = "myorg/repo1#23725"
		leaseKey = "myorg/repo1!myorg/repo1#23725:implement"
		taskID   = "task-23725"
	)
	now := time.Now().UTC()
	if err := s.contributeHub.recordLeaseForKeyStage("c-63e2b14bad27#copilot", taskID, repo, 23725, leaseKey, "trusted", StageImplement, 8, now); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	s.status = &StatusPayload{Repos: []FrontendRepo{{
		Name: "repo1",
		Full: repo,
		ActionableIssues: []any{map[string]any{
			"number":   23725,
			"title":    "Implement campaign run detail view",
			"html_url": "https://github.com/myorg/repo1/issues/23725",
		}},
	}}}
	if _, err := writeStageReceipt(key, StagePlan, 7, []byte(`{"stage":"plan","generation":7,"output_digest":"sha256:plan","started_at":"2026-09-25T23:00:00Z","ended_at":"2026-09-25T23:05:00Z","result_class":"completed"}`)); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	if err := writeSpekStageCapture(key, StagePlan, 7, RunDetailStageCapture{
		Artifact:        "20260925212730-myorg-repo1-23725",
		StartedAt:       "2026-09-25T23:00:01Z",
		EndedAt:         "2026-09-25T23:05:02Z",
		Prompt:          &RunDetailTextBlock{Text: "author the plan"},
		AgentTranscript: &RunDetailTextBlock{Text: "answered clarification questions"},
		StatusHistory:   []RunDetailStageStatus{{At: "2026-09-25T23:05:02Z", Step: "finished", DocumentStatus: "final", CompletedSteps: []string{"interview"}}},
		Interview:       []RunDetailInterview{{Step: "interview", Question: "What should happen?", Answer: "Render prose.", AnsweredAt: "2026-09-25T23:04:00Z"}},
		Documents:       []RunDetailStageDocument{{Path: "20260925212730-myorg-repo1-23725/plan.md", Markdown: "# Plan\n\nDo the work.", Content: "# Plan\n\nDo the work."}},
	}); err != nil {
		t.Fatalf("write capture: %v", err)
	}
	appendRunDetailEvent(key, runDetailPersistedEvent{
		TS:      now.Format(time.RFC3339),
		Kind:    "task_complete",
		TaskID:  taskID,
		Stage:   StageImplement,
		Gen:     8,
		Actor:   "c-63e2b14bad27#copilot",
		Backend: "copilot",
		Model:   "claude-fable-5.1",
		Summary: "opened implementation PR",
		PRURL:   "https://github.com/myorg/repo1/pull/23743",
		Reason:  "GitHub App quota exhausted",
	})
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: key,
		Kind:     timeline.KindProgress,
		Agent:    "hive-spektacular-hub",
		At:       now.Add(-time.Minute).UnixMilli(),
		Attrs:    map[string]string{"stage": StagePlan, "gen": "7", "event": "cli_exited", "backend": "copilot"},
	})
	taskRunMu.Lock()
	if err := os.WriteFile(taskRunLogPath, []byte(`{"ts":"2026-09-25T23:38:04Z","task_id":"task-23725","task_gen":8,"repo":"myorg/repo1","number":23725,"username":"c-63e2b14bad27#copilot","backend":"copilot","model":"claude-fable-5.1","outcome":"completed","reported_pr_url":"https://github.com/myorg/repo1/pull/23743","pr_verified":false,"pr_verify_reason":"GitHub App quota exhausted","scenario":"headless_complete"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write task run log: %v", err)
	}
	taskRunMu.Unlock()

	rec := doGet(s, "/api/runs/"+url.PathEscape(key)+"/detail")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var detail RunDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Issue.Title != "Implement campaign run detail view" || detail.Issue.URL == "" {
		t.Fatalf("issue = %+v", detail.Issue)
	}
	if len(detail.PRs) == 0 || detail.PRs[0].URL != "https://github.com/myorg/repo1/pull/23743" || detail.PRs[0].Verified {
		t.Fatalf("prs = %+v", detail.PRs)
	}
	body := string(rec.Body.Bytes())
	for _, want := range []string{"sha256:plan", "cli_exited", "opened implementation PR", "GitHub App quota exhausted", "What should happen?", "# Plan", "answered clarification questions"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail body missing %q: %s", want, body)
		}
	}
	for _, st := range detail.Stages {
		if st.Name == StagePlan {
			if st.StartedAt != "2026-09-25T23:00:01Z" || len(st.Interview) != 1 || len(st.Documents) != 1 || st.Documents[0].Content == "" || st.AgentTranscript == nil {
				t.Fatalf("plan stage missing capture: %+v", st)
			}
		}
	}
}

func TestRunDetailReadRoleAllowed(t *testing.T) {
	s, _ := runsTestServer(t)
	if err := s.contributeHub.recordLeaseForKeyStage("agent", "task", "myorg/repo1", 42, "myorg/repo1!myorg/repo1#42:spec", "trusted", StageSpec, 1, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/myorg%2Frepo1%2342/detail", nil)
	req.Header.Set("X-Hive-Role", config.RoleRead)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read role detail = %d body=%s", rec.Code, rec.Body.String())
	}
}

// The lease generation advances past the generation a stage's capture was
// written under (spec gen1 finishes, the same lease becomes plan gen2 and the
// spec stage reports gen2). The capture must still attach.
func TestBuildRunDetailStagesAttachesCaptureOlderThanLeaseGen(t *testing.T) {
	run := Run{Key: "acme/repo#7", Stages: []RunStage{{Name: StageSpec, Status: "completed", Gen: 2}, {Name: StagePlan, Status: "current", Gen: 2}}}
	captures := []RunDetailStageCapture{
		{Stage: StageSpec, Generation: 1, Capture: spekCaptureAlreadyFinal, Documents: []RunDetailStageDocument{{Path: "shortcut.md", Content: "shortcut"}}},
		{Stage: StageSpec, Generation: 1, Capture: "session", Documents: []RunDetailStageDocument{{Path: "spec.md", Content: "# real spec"}}, Interview: []RunDetailInterview{{Step: "interview", Question: "Who is the audience?", Answer: "Operators"}}},
	}
	stages := buildRunDetailStages(run, nil, nil, captures, nil, nil)
	var spec *RunDetailStage
	for i := range stages {
		if stages[i].Name == StageSpec {
			spec = &stages[i]
		}
	}
	if spec == nil {
		t.Fatalf("spec stage missing: %+v", stages)
	}
	if len(spec.Documents) != 1 || spec.Documents[0].Content != "# real spec" {
		t.Fatalf("expected the real session capture to attach, got documents %+v", spec.Documents)
	}
	if len(spec.Interview) != 1 || spec.Interview[0].Answer != "Operators" {
		t.Fatalf("expected interview from capture, got %+v", spec.Interview)
	}
	if spec.Gen != 2 {
		t.Fatalf("lease gen must not regress, got %d", spec.Gen)
	}
	for _, m := range spec.Missing {
		if strings.Contains(m, "were not captured") {
			t.Fatalf("stage wrongly reported as uncaptured: %q", m)
		}
	}
}

func TestLatestRunDetailCapturesPrefersHighestGenThenSession(t *testing.T) {
	got := latestRunDetailCaptures([]RunDetailStageCapture{
		{Stage: StageSpec, Generation: 1, Capture: "session"},
		{Stage: StageSpec, Generation: 3, Capture: spekCaptureAlreadyFinal},
		{Stage: StageSpec, Generation: 3, Capture: "session", Artifact: "winner"},
		{Stage: StagePlan, Generation: 2, Capture: "session", Artifact: "plan"},
	})
	if len(got) != 2 || got[0].Stage != StageSpec || got[0].Artifact != "winner" || got[1].Artifact != "plan" {
		t.Fatalf("unexpected selection: %+v", got)
	}
}

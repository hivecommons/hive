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
		Documents:       []RunDetailStageDocument{{Path: "20260925212730-myorg-repo1-23725/plan.md", Markdown: "# Plan\n\nDo the work."}},
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
			if st.StartedAt != "2026-09-25T23:00:01Z" || len(st.Interview) != 1 || len(st.Documents) != 1 || st.AgentTranscript == nil {
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

package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

func runInterviewTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	s, _ := runsTestServer(t)
	oldRoot := agentWorkspaceRoot
	agentWorkspaceRoot = t.TempDir()
	t.Cleanup(func() { agentWorkspaceRoot = oldRoot })
	runKey := "myorg/repo1#9024"
	identity := config.DefaultSpektacularHubExecutorIdentity
	if err := s.contributeHub.recordLeaseForKeyStage(identity, "run-hub-9024", "myorg/repo1", 9024, runKey, "trusted", StageSpec, 4, time.Now()); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	worktree := spekHubRunWorktreePath(identity, runKey)
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := SpekInterviewRequest{
		SchemaVersion: spekInterviewSchemaVersion,
		Stage:         StageSpec,
		Artifact:      "myorg-repo1-9024",
		Step:          "interview",
		Questions: []SpekInterviewQuestion{
			{ID: "scope", Text: "What scope should Spek cover?", Context: "Needed before drafting."},
			{ID: "risk", Text: "Which risk matters most?", Options: []string{"security", "latency"}},
		},
	}
	data, _ := json.Marshal(req)
	if err := os.WriteFile(filepath.Join(worktree, spekInterviewRequestRelPath), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return s, runKey, worktree
}

func TestRunInterviewAPIListsPendingQuestionsAndRunState(t *testing.T) {
	s, runKey, _ := runInterviewTestServer(t)

	rec := doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/interview")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET interview = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload RunInterviewPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Pending != 2 || payload.Step != "interview" || len(payload.Questions) != 2 {
		t.Fatalf("payload = %+v, want two pending interview questions", payload)
	}

	list := doOwnerGet(s, "/api/runs")
	if list.Code != http.StatusOK {
		t.Fatalf("GET runs = %d body=%s", list.Code, list.Body.String())
	}
	var runs []Run
	if err := json.Unmarshal(list.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	if len(runs) != 1 || runs[0].WaitingOn != RunWaitingOnHuman || runs[0].WaitingReason != worksource.RunWaitingReasonInterviewQuestions || runs[0].Interview == nil || runs[0].Interview.Pending != 2 {
		t.Fatalf("run state = %+v, want human interview hold", runs)
	}
}

func TestRunInterviewPostWritesAnswersAndClearsPending(t *testing.T) {
	s, runKey, worktree := runInterviewTestServer(t)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/interview", runInterviewPostRequest{Answers: []SpekInterviewAnswer{{ID: "scope", Answer: "Only dashboard flows."}, {ID: "risk", Answer: "latency"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST interview = %d body=%s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(worktree, spekInterviewAnswersRelPath))
	if err != nil {
		t.Fatalf("answers file missing: %v", err)
	}
	var answers SpekInterviewAnswers
	if err := json.Unmarshal(data, &answers); err != nil {
		t.Fatalf("decode answers: %v", err)
	}
	if len(answers.Answers) != 2 || answers.Answers[0].Actor == "" {
		t.Fatalf("answers = %+v, want attributed human answers", answers.Answers)
	}
	rec = doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/interview")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after answers = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload RunInterviewPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Pending != 0 || len(payload.Questions) != 0 || len(payload.Answers) != 2 {
		t.Fatalf("payload after answers = %+v, want answered history and no pending", payload)
	}
}

func TestRunInterviewPostRequiresAllPendingAnswers(t *testing.T) {
	s, runKey, _ := runInterviewTestServer(t)

	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/interview", runInterviewPostRequest{Answers: []SpekInterviewAnswer{{ID: "scope", Answer: "Only dashboard flows."}}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("partial POST interview = %d body=%s, want 400", rec.Code, rec.Body.String())
	}

	rec = doOwnerGet(s, "/api/runs/"+url.PathEscape(runKey)+"/interview")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after partial = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload RunInterviewPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Pending != 2 {
		t.Fatalf("pending after partial = %d, want 2", payload.Pending)
	}
}

func TestSpekInterviewHelpersEmbedAnswersAndCaptureHumanSource(t *testing.T) {
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, ".hive"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := SpekInterviewRequest{
		SchemaVersion: spekInterviewSchemaVersion,
		Stage:         StagePlan,
		Artifact:      "artifact-1",
		Step:          "clarify",
		Questions: []SpekInterviewQuestion{
			{ID: "one", Text: "First?"},
			{ID: "two", Text: "Second?"},
		},
	}
	reqData, _ := json.Marshal(req)
	if err := os.WriteFile(filepath.Join(worktree, spekInterviewRequestRelPath), reqData, 0o600); err != nil {
		t.Fatal(err)
	}
	answers := SpekInterviewAnswers{SchemaVersion: spekInterviewSchemaVersion, Answers: []SpekInterviewAnswer{{ID: "one", Answer: "A1", Actor: "owner"}}}
	answerData, _ := json.Marshal(answers)
	if err := os.WriteFile(filepath.Join(worktree, spekInterviewAnswersRelPath), answerData, 0o600); err != nil {
		t.Fatal(err)
	}

	block := spekInterviewPromptBlock(StagePlan, "artifact-1", readSpekInterviewAnswers(worktree))
	if !strings.Contains(block, "MUST NOT answer") || !strings.Contains(block, `"answer":"A1"`) {
		t.Fatalf("prompt block did not include protocol and answers:\n%s", block)
	}
	_, gotAnswers, pending, _, ok := spekInterviewPending(worktree)
	if !ok || len(gotAnswers.Answers) != 1 || len(pending) != 1 || pending[0].ID != "two" {
		t.Fatalf("pending=%+v answers=%+v ok=%v, want only second question pending", pending, gotAnswers.Answers, ok)
	}
	entries := appendSpekHumanInterview(worktree, nil, "2026-09-26T19:00:00Z")
	if len(entries) != 1 || entries[0].Source != "human:owner" || entries[0].Question != "First?" || entries[0].Answer != "A1" {
		t.Fatalf("entries = %+v, want attributed human interview", entries)
	}
}

func TestRunInterviewErrorAndAnsweredBranches(t *testing.T) {
	s, runKey, worktree := runInterviewTestServer(t)

	if rec := doPostNoOwner(s, "/api/runs/"+url.PathEscape(runKey)+"/interview", runInterviewPostRequest{Answers: []SpekInterviewAnswer{{ID: "scope", Answer: "x"}, {ID: "risk", Answer: "y"}}}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner POST = %d, want 403", rec.Code)
	}
	if rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/interview", map[string]any{"answers": []map[string]string{{"id": "missing", "answer": "x"}}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown answer POST = %d, want 400", rec.Code)
	}
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(runKey)+"/interview", runInterviewPostRequest{Answers: []SpekInterviewAnswer{{ID: "scope", Answer: "dashboard"}, {ID: "risk", Answer: "security"}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST all answers = %d body=%s", rec.Code, rec.Body.String())
	}
	payload, err := s.RunInterviewPayload(runKey)
	if err != nil {
		t.Fatalf("RunInterviewPayload after answers: %v", err)
	}
	if payload.Pending != 0 || len(payload.Questions) != 0 || len(payload.Answers) != 2 {
		t.Fatalf("answered payload = %+v", payload)
	}
	runs, err := s.activeRuns(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].WaitingOn == RunWaitingOnHuman || runs[0].Interview != nil {
		t.Fatalf("answered run should not be held on interview: %+v", runs)
	}
	if _, err := s.spekInterviewWorktree("missing/run#1"); !errors.Is(err, errRunInterviewNotFound) {
		t.Fatalf("missing worktree err = %v, want errRunInterviewNotFound", err)
	}
	if data := readSpekInterviewAnswers(filepath.Join(worktree, "missing")); data != nil {
		t.Fatalf("missing answers read = %q, want nil", data)
	}
	if got := runInterviewDashboardURL(runKey); !strings.Contains(got, "%23") || !strings.Contains(got, "#interview") {
		t.Fatalf("dashboard url = %q", got)
	}
	if runInterviewStatus(errors.New("boom")) != http.StatusInternalServerError {
		t.Fatal("generic interview error should map to 500")
	}
	e := &SpekHubExecutor{Config: config.RunsConfig{Spektacular: config.SpektacularConfig{Interview: "auto"}}}
	if e.stageInterviewMode() != "auto" || ((*SpekHubExecutor)(nil)).stageInterviewMode() != "human" {
		t.Fatal("unexpected stage interview modes")
	}
}

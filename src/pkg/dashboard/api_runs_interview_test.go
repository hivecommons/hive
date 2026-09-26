package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

package dashboard

// Tests for hive#7317: the per-contributor run-history read and the durable
// hand-back records. Together they pin the two halves of "an operator can
// troubleshoot a struggling contributor from the API": the reasons in
// task_runs.jsonl are reachable per-user, and the hand-back paths that used to
// write nothing now write records.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDeriveScenario_Abandoned(t *testing.T) {
	// An abandoned outcome maps to its own scenario regardless of
	// signal/kind — nothing terminal ever arrived, so neither applies.
	if got := deriveScenario(outcomeAbandoned, "verdict", TaskFailureKindTask); got != scenarioAbandoned {
		t.Errorf("deriveScenario(abandoned) = %q, want %q", got, scenarioAbandoned)
	}
}

// writeRunRecords appends pre-stamped records straight to the scratch log.
func writeRunRecords(t *testing.T, path string, recs []TaskRunRecord) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open scratch log: %v", err)
	}
	defer f.Close()
	for _, rec := range recs {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestReadTaskRuns_FiltersWindowsAndLimits(t *testing.T) {
	path := scratchRunLog(t)
	now := time.Now().UTC()
	writeRunRecords(t, path, []TaskRunRecord{
		{TS: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339), Username: "alice", TaskID: "old", Outcome: "failed", Scenario: scenarioEnvFailure},
		{TS: now.Add(-2 * time.Hour).Format(time.RFC3339), Username: "alice", TaskID: "t1", Outcome: "failed", Reason: "boom", Scenario: scenarioEnvFailure},
		{TS: now.Add(-1 * time.Hour).Format(time.RFC3339), Username: "bob", TaskID: "t2", Outcome: "completed", Scenario: scenarioVerdictComplete},
		{TS: now.Add(-30 * time.Minute).Format(time.RFC3339), Username: "Alice", TaskID: "t3", Outcome: outcomeAbandoned, Reason: taskRunReasonReadvertised, Scenario: scenarioAbandoned},
	})

	runs, total, err := readTaskRuns(path, "alice", 7*24*time.Hour, 50)
	if err != nil {
		t.Fatalf("readTaskRuns: %v", err)
	}
	if total != 2 || len(runs) != 2 {
		t.Fatalf("want 2 alice records in window, got total=%d len=%d: %+v", total, len(runs), runs)
	}
	// Newest first, case-insensitive username match.
	if runs[0].TaskID != "t3" || runs[1].TaskID != "t1" {
		t.Errorf("want newest-first [t3 t1], got [%s %s]", runs[0].TaskID, runs[1].TaskID)
	}
	if runs[1].Reason != "boom" {
		t.Errorf("reason must survive the read, got %q", runs[1].Reason)
	}

	// limit truncates but total still reports the full window count.
	runs, total, err = readTaskRuns(path, "alice", 7*24*time.Hour, 1)
	if err != nil {
		t.Fatalf("readTaskRuns limited: %v", err)
	}
	if total != 2 || len(runs) != 1 || runs[0].TaskID != "t3" {
		t.Errorf("limit=1: want total=2 and newest record only, got total=%d runs=%+v", total, runs)
	}

	// window=0 means everything in the file.
	_, total, err = readTaskRuns(path, "alice", 0, 50)
	if err != nil {
		t.Fatalf("readTaskRuns unwindowed: %v", err)
	}
	if total != 3 {
		t.Errorf("window=0: want all 3 alice records, got %d", total)
	}

	// A missing file is an empty history, not an error.
	runs, total, err = readTaskRuns(path+".nope", "alice", 0, 50)
	if err != nil || total != 0 || len(runs) != 0 {
		t.Errorf("missing file: want empty, got runs=%v total=%d err=%v", runs, total, err)
	}
}

func TestHandleContributeRuns_Endpoint(t *testing.T) {
	path := scratchRunLog(t)
	s, ts := setupWSTest(t)
	defer ts.Close()

	now := time.Now().UTC()
	writeRunRecords(t, path, []TaskRunRecord{
		{TS: now.Add(-1 * time.Hour).Format(time.RFC3339), Username: "carol", TaskID: "c1", Outcome: "failed", FailureKind: TaskFailureKindEnvironment, Reason: "CLI never became ready", Scenario: scenarioEnvFailure, DurationS: 601},
		{TS: now.Add(-30 * time.Minute).Format(time.RFC3339), Username: "carol", TaskID: "c2", Outcome: outcomeAbandoned, Reason: taskRunReasonConnectionLost, Scenario: scenarioAbandoned},
	})

	// username is required: without it the endpoint would dump the whole log.
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/runs", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no username: want 400, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=carol&days=1", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("runs status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Username   string          `json:"username"`
		WindowDays int             `json:"window_days"`
		Total      int             `json:"total"`
		Runs       []TaskRunRecord `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("runs response: %v", err)
	}
	if resp.Username != "carol" || resp.WindowDays != 1 || resp.Total != 2 || len(resp.Runs) != 2 {
		t.Fatalf("response shape wrong: %+v", resp)
	}
	if resp.Runs[0].TaskID != "c2" || resp.Runs[0].Reason != taskRunReasonConnectionLost {
		t.Errorf("newest-first with reason expected, got %+v", resp.Runs[0])
	}
	if resp.Runs[1].Reason != "CLI never became ready" || resp.Runs[1].FailureKind != TaskFailureKindEnvironment {
		t.Errorf("failure detail must be served, got %+v", resp.Runs[1])
	}
}

// pollRunLogLines waits for the scratch log to contain want complete records.
// The WS handlers write asynchronously relative to the test goroutine (#6453).
func pollRunLogLines(t *testing.T, path string, want int) []TaskRunRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) >= want && strings.TrimSpace(lines[len(lines)-1]) != "" {
				recs := make([]TaskRunRecord, 0, len(lines))
				ok := true
				for _, line := range lines {
					var rec TaskRunRecord
					if json.Unmarshal([]byte(line), &rec) != nil {
						ok = false
						break
					}
					recs = append(recs, rec)
				}
				if ok && len(recs) >= want {
					return recs
				}
			}
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(path)
			t.Fatalf("run log did not reach %d records within 5s: %q", want, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A relay that sends "ready" while still holding a task hands the task back
// with no terminal report. hive#7317: that hand-back must leave a durable run
// record — before this, 10 of 11 tasks in the motivating incident left nothing.
func TestTaskRunLog_RecordedOnReadyHandBack(t *testing.T) {
	path := scratchRunLog(t)
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"handback-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude", Model: "claude-haiku-4-5"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(71, "handback issue", time.Now().Add(-24*time.Hour)),
				},
			},
		},
	}
	s.statusMu.Unlock()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	// Hand it straight back by re-advertising ready while still holding it.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})

	recs := pollRunLogLines(t, path, 1)
	rec := recs[0]
	if rec.Username != "handback-user" || rec.Backend != "claude" {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.Outcome != outcomeAbandoned || rec.Scenario != scenarioAbandoned {
		t.Errorf("outcome/scenario wrong: %+v", rec)
	}
	if rec.Reason != taskRunReasonReadvertised {
		t.Errorf("reason = %q, want %q", rec.Reason, taskRunReasonReadvertised)
	}
	if rec.Repo != "myorg/repo1" || rec.Number != 71 {
		t.Errorf("task identity wrong: %+v", rec)
	}
	if rec.DurationS <= 0 {
		t.Errorf("duration_s must be positive (assignment→hand-back), got %v", rec.DurationS)
	}

	// The record must be reachable through the per-contributor read.
	runs, total, err := readTaskRuns(path, "handback-user", 24*time.Hour, 10)
	if err != nil || total != 1 || len(runs) != 1 {
		t.Fatalf("readTaskRuns after hand-back: runs=%v total=%d err=%v", runs, total, err)
	}
}

// A socket that drops while holding a task takes tmux_output and last_failure
// with it; the run record is the piece the hub can still keep. hive#7317.
func TestTaskRunLog_RecordedOnDisconnectRelease(t *testing.T) {
	path := scratchRunLog(t)
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"dropped-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "copilot", Model: "gpt-5"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(72, fmt.Sprintf("disconnect issue %d", 72), time.Now().Add(-24*time.Hour)),
				},
			},
		},
	}
	s.statusMu.Unlock()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	// Drop the socket while holding the task.
	conn.Close()

	recs := pollRunLogLines(t, path, 1)
	rec := recs[0]
	if rec.Username != "dropped-user" || rec.Backend != "copilot" {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.Outcome != outcomeAbandoned || rec.Scenario != scenarioAbandoned {
		t.Errorf("outcome/scenario wrong: %+v", rec)
	}
	if rec.Reason != taskRunReasonConnectionLost {
		t.Errorf("reason = %q, want %q", rec.Reason, taskRunReasonConnectionLost)
	}
	if rec.Repo != "myorg/repo1" || rec.Number != 72 {
		t.Errorf("task identity wrong: %+v", rec)
	}
}

// Abandoned records fold into run-stats as failures with their own scenario —
// the direction hive#7317 calls out: totals currently undercount for exactly
// the sessions where everything is handed back.
func TestReadTaskRunStats_CountsAbandoned(t *testing.T) {
	path := scratchRunLog(t)
	now := time.Now().UTC()
	writeRunRecords(t, path, []TaskRunRecord{
		{TS: now.Format(time.RFC3339), Username: "a", Backend: "litellm", TaskID: "t1", Outcome: "failed", Scenario: scenarioEnvFailure},
		{TS: now.Format(time.RFC3339), Username: "a", Backend: "litellm", TaskID: "t2", Outcome: outcomeAbandoned, Scenario: scenarioAbandoned},
		{TS: now.Format(time.RFC3339), Username: "a", Backend: "litellm", TaskID: "t3", Outcome: outcomeAbandoned, Scenario: scenarioAbandoned},
	})
	stats, total, err := readTaskRunStats(path, 24*time.Hour)
	if err != nil {
		t.Fatalf("readTaskRunStats: %v", err)
	}
	if total != 3 || len(stats) != 1 {
		t.Fatalf("want 3 records for one backend, got total=%d stats=%+v", total, stats)
	}
	st := stats[0]
	if st.Failed != 3 || st.Completed != 0 {
		t.Errorf("abandoned must count as failed in aggregates: %+v", st)
	}
	if st.Scenarios[scenarioAbandoned] != 2 {
		t.Errorf("scenario counts wrong: %+v", st.Scenarios)
	}
}

package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Redirects the run log to a scratch file for one test.
func scratchRunLog(t *testing.T) string {
	t.Helper()
	prev := taskRunLogPath
	taskRunLogPath = t.TempDir() + "/task_runs.jsonl"
	t.Cleanup(func() { taskRunLogPath = prev })
	return taskRunLogPath
}

// The scenario vocabulary is the ratchet axis: every (outcome, signal, kind)
// combination must map deterministically, and unknown inputs must land in a
// bucket rather than invent a new one.
func TestDeriveScenario_Table(t *testing.T) {
	cases := []struct {
		outcome, signal, kind, want string
	}{
		{"completed", completionSignalVerdict, "", scenarioVerdictComplete},
		{"completed", completionSignalChromeIdle, "", scenarioIdleComplete},
		// No signal on the wire: the headless one-shot path, or a pre-#5376
		// relay — both normalize to "unknown" before reaching the log.
		{"completed", completionSignalUnknown, "", scenarioHeadlessComplete},
		{"completed", "", "", scenarioHeadlessComplete},
		{"failed", "", TaskFailureKindEnvironment, scenarioEnvFailure},
		{"failed", "", TaskFailureKindTask, scenarioTaskFailure},
		{"failed", "", TaskFailureKindUnspecified, scenarioUnspecifiedFailure},
		{"failed", "", "", scenarioUnspecifiedFailure},
		// A failure's stray completion signal must not smuggle it into a
		// completion scenario.
		{"failed", completionSignalVerdict, TaskFailureKindTask, scenarioTaskFailure},
	}
	for _, tc := range cases {
		if got := deriveScenario(tc.outcome, tc.signal, tc.kind); got != tc.want {
			t.Errorf("deriveScenario(%q,%q,%q) = %q, want %q",
				tc.outcome, tc.signal, tc.kind, got, tc.want)
		}
	}
}

func TestTaskRunLog_AppendStampsAndDefaults(t *testing.T) {
	path := scratchRunLog(t)
	var hub *ContributeWSHub // nil hub must be tolerated (best-effort contract)
	hub.appendTaskRun(TaskRunRecord{
		TaskID:           "ct-1",
		Username:         "alice",
		Backend:          "claude",
		Outcome:          "completed",
		CompletionSignal: completionSignalVerdict,
		DurationS:        12.5,
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var rec TaskRunRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.TS == "" {
		t.Error("append must stamp ts")
	}
	if rec.Scenario != scenarioVerdictComplete {
		t.Errorf("scenario = %q, want %q", rec.Scenario, scenarioVerdictComplete)
	}
	if rec.Session != "ct-1" {
		t.Errorf("session must default to the task id, got %q", rec.Session)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log must be 0600, got %v (err %v)", fi.Mode(), err)
	}
}

func TestTaskRunLog_RotatesAtCap(t *testing.T) {
	path := scratchRunLog(t)
	// A live file already at the cap must be rotated to .1 before the append,
	// and a previous .1 replaced — disk use stays bounded at two files.
	if err := os.MkdirAll(t.TempDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, taskRunLogMaxBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("old rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var hub *ContributeWSHub
	hub.appendTaskRun(TaskRunRecord{TaskID: "ct-2", Outcome: "failed"})

	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	if len(live) >= taskRunLogMaxBytes {
		t.Fatalf("live file was not rotated: %d bytes", len(live))
	}
	var rec TaskRunRecord
	if err := json.Unmarshal(live, &rec); err != nil || rec.TaskID != "ct-2" {
		t.Fatalf("fresh record must land in the fresh file: %v %+v", err, rec)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != int64(taskRunLogMaxBytes) {
		t.Fatalf(".1 must hold the rotated file, got %v (err %v)", fi, err)
	}
}

func TestReadTaskRunStats_Aggregates(t *testing.T) {
	path := scratchRunLog(t)
	now := time.Now().UTC()
	mk := func(age time.Duration, backend, outcome, signal, kind string, dur float64) string {
		rec := TaskRunRecord{
			TS:        now.Add(-age).Format(time.RFC3339),
			TaskID:    "x",
			Backend:   backend,
			Outcome:   outcome,
			Scenario:  deriveScenario(outcome, signal, kind),
			DurationS: dur,
		}
		b, _ := json.Marshal(rec)
		return string(b)
	}
	lines := []string{
		mk(time.Hour, "claude", "completed", completionSignalVerdict, "", 100),
		mk(time.Hour, "claude", "completed", completionSignalChromeIdle, "", 300),
		mk(time.Hour, "claude", "failed", "", TaskFailureKindEnvironment, 0),
		mk(time.Hour, "codex", "completed", "", "", 50),
		// Outside a 1-day window; must be excluded from a windowed read.
		mk(48*time.Hour, "claude", "completed", completionSignalVerdict, "", 10),
		`{"torn json`, // a torn tail line must not poison the aggregate
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, total, err := readTaskRunStats(path, 24*time.Hour)
	if err != nil {
		t.Fatalf("readTaskRunStats: %v", err)
	}
	if total != 4 {
		t.Fatalf("windowed total = %d, want 4", total)
	}
	if len(stats) != 2 || stats[0].Backend != "claude" || stats[1].Backend != "codex" {
		t.Fatalf("backends wrong: %+v", stats)
	}
	cl := stats[0]
	if cl.Completed != 2 || cl.Failed != 1 {
		t.Errorf("claude completed/failed = %d/%d, want 2/1", cl.Completed, cl.Failed)
	}
	if cl.Scenarios[scenarioIdleComplete] != 1 || cl.Scenarios[scenarioEnvFailure] != 1 {
		t.Errorf("claude scenarios wrong: %+v", cl.Scenarios)
	}
	if cl.ChromeIdleShare != 0.5 {
		t.Errorf("claude chrome_idle_share = %v, want 0.5", cl.ChromeIdleShare)
	}
	if cl.DurationP50S != 300 { // sorted [100 300], len/2 = index 1
		t.Errorf("claude p50 = %v, want 300", cl.DurationP50S)
	}

	// Unwindowed read sees everything parseable.
	_, allTotal, err := readTaskRunStats(path, 0)
	if err != nil || allTotal != 5 {
		t.Fatalf("unwindowed total = %d (err %v), want 5", allTotal, err)
	}

	// A missing file is an empty aggregate, not an error.
	if s, n, err := readTaskRunStats(path+".missing", 0); err != nil || n != 0 || len(s) != 0 {
		t.Fatalf("missing file must aggregate to empty: %v %d %v", s, n, err)
	}
}

// Drives the REAL WebSocket task_complete handler end to end (the same
// harness as TestNoWorkVerdict_NeverGrantsTrustCredit) and asserts the run
// log gains exactly one record carrying the normalized fields and a real
// duration — the two things nothing durably recorded before.
func TestTaskRunLog_RecordedOnCompletion(t *testing.T) {
	path := scratchRunLog(t)
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"runlog-user"}`
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
					noWorkIssue(91, "runlog issue", time.Now().Add(-24*time.Hour)),
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
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Result: "completed", Verdict: "no_work_needed", VerdictReason: "smoke",
		CompletionSignal: "verdict",
	})

	// The handler writes the record asynchronously relative to this
	// goroutine, and appendTaskRun O_CREATEs the file before writing, so a
	// fixed sleep can observe an empty or absent file on a loaded runner
	// (#6453). Poll for a complete newline-terminated record instead.
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte("\n")) {
			data = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run log not written within 5s: err=%v partial=%q", err, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one record, got %d: %q", len(lines), data)
	}
	var rec TaskRunRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Username != "runlog-user" || rec.Backend != "claude" || rec.Model != "claude-haiku-4-5" {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.Outcome != "completed" || rec.Scenario != scenarioVerdictComplete {
		t.Errorf("outcome/scenario wrong: %+v", rec)
	}
	if rec.Repo != "myorg/repo1" || rec.Number != 91 {
		t.Errorf("task identity wrong: %+v", rec)
	}
	if rec.Verdict != "no_work_needed" {
		t.Errorf("verdict = %q, want no_work_needed", rec.Verdict)
	}
	if rec.DurationS <= 0 {
		t.Errorf("duration_s must be positive (assignment→completion), got %v", rec.DurationS)
	}
	if rec.PRVerified {
		t.Errorf("no PR was reported, pr_verified must be false: %+v", rec)
	}

	// And the aggregate endpoint sees it.
	req2 := httptest.NewRequest(http.MethodGet, "/api/contribute/run-stats?days=1", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("run-stats status %d", w2.Code)
	}
	var resp struct {
		Total    int                   `json:"total"`
		Backends []taskRunBackendStats `json:"backends"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("run-stats response: %v", err)
	}
	if resp.Total != 1 || len(resp.Backends) != 1 || resp.Backends[0].Backend != "claude" ||
		resp.Backends[0].Scenarios[scenarioVerdictComplete] != 1 {
		t.Fatalf("run-stats aggregate wrong: %+v", resp)
	}
}

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

	"github.com/hivecommons/hive/internal/testutil"
)

// Redirects the run log to a scratch file for one test.
//
// The swap and the restore both hold taskRunMu: a hijacked websocket handler
// can outlive its test (httptest's Close does not wait for hijacked conns),
// and its deferred disconnect-abandonment append reads taskRunLogPath under
// that mutex — an unguarded restore in Cleanup races with it.
func scratchRunLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	taskRunMu.Lock()
	prev := taskRunLogPath
	taskRunLogPath = dir + "/task_runs.jsonl"
	path := taskRunLogPath
	taskRunMu.Unlock()
	t.Cleanup(func() {
		taskRunMu.Lock()
		taskRunLogPath = prev
		taskRunMu.Unlock()
	})
	return path
}

// The scenario vocabulary is the ratchet axis: every (outcome, signal, kind)
// combination must map deterministically, and unknown inputs must land in a
// bucket rather than invent a new one.
func TestDeriveScenario_Table(t *testing.T) {
	cases := []struct {
		outcome, signal, kind, cause, want string
	}{
		{"completed", completionSignalVerdict, "", "", scenarioVerdictComplete},
		{"completed", completionSignalChromeIdle, "", "", scenarioIdleComplete},
		// No signal on the wire: the headless one-shot path, or a pre-#5376
		// relay — both normalize to "unknown" before reaching the log.
		{"completed", completionSignalUnknown, "", "", scenarioHeadlessComplete},
		{"completed", "", "", "", scenarioHeadlessComplete},
		{"failed", "", TaskFailureKindEnvironment, "", scenarioEnvFailure},
		{"failed", "", TaskFailureKindTask, "", scenarioTaskFailure},
		{"failed", "", TaskFailureKindUnspecified, "", scenarioUnspecifiedFailure},
		{"failed", "", "", "", scenarioUnspecifiedFailure},
		// A failure's stray completion signal must not smuggle it into a
		// completion scenario.
		{"failed", completionSignalVerdict, TaskFailureKindTask, "", scenarioTaskFailure},
		// #7317: abandonment keys on the hub-observed cause, never on the
		// client-reported kind or signal.
		{outcomeAbandoned, "", "", abandonCauseHandback, scenarioAbandonedHandback},
		{outcomeAbandoned, "", "", abandonCauseDisconnect, scenarioAbandonedDisconnect},
		{outcomeAbandoned, "", "", "", scenarioAbandonedOther},
		{outcomeAbandoned, "", "", "something-new", scenarioAbandonedOther},
		// An abandonment carrying stray client fields must not be reclassified
		// as a completion or a failure — the outcome decides, first.
		{outcomeAbandoned, completionSignalVerdict, TaskFailureKindTask, abandonCauseHandback, scenarioAbandonedHandback},
	}
	for _, tc := range cases {
		if got := deriveScenario(tc.outcome, tc.signal, tc.kind, tc.cause); got != tc.want {
			t.Errorf("deriveScenario(%q,%q,%q,%q) = %q, want %q",
				tc.outcome, tc.signal, tc.kind, tc.cause, got, tc.want)
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
			Scenario:  deriveScenario(outcome, signal, kind, ""),
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

// #7317: abandonment rows are new to this log, so they must not silently move
// the two numbers the backend ratchet already reads. They get their own bucket
// rather than joining Failed, and their durations stay out of the percentile
// pool — a 26-minute pane stall is not evidence about how long this backend's
// real runs take.
func TestReadTaskRunStats_AbandonedIsItsOwnBucket(t *testing.T) {
	path := scratchRunLog(t)
	now := time.Now().UTC()
	mk := func(outcome, cause string, dur float64) string {
		rec := TaskRunRecord{
			TS:           now.Add(-time.Hour).Format(time.RFC3339),
			TaskID:       "x",
			Backend:      "litellm",
			Outcome:      outcome,
			AbandonCause: cause,
			Scenario:     deriveScenario(outcome, "", "", cause),
			DurationS:    dur,
		}
		b, _ := json.Marshal(rec)
		return string(b)
	}
	lines := []string{
		mk(outcomeCompleted, "", 100),
		mk(outcomeFailed, "", 200),
		// The shape of the session in #7317: repeated hand-backs on the relay's
		// own timeouts, plus one lost socket.
		mk(outcomeAbandoned, abandonCauseHandback, 1560),
		mk(outcomeAbandoned, abandonCauseHandback, 600),
		mk(outcomeAbandoned, abandonCauseDisconnect, 0),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, total, err := readTaskRunStats(path, 24*time.Hour)
	if err != nil || len(stats) != 1 {
		t.Fatalf("readTaskRunStats: %v %+v", err, stats)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 — abandonments are runs and do count", total)
	}
	st := stats[0]
	if st.Completed != 1 {
		t.Errorf("completed = %d, want 1", st.Completed)
	}
	if st.Failed != 1 {
		t.Errorf("failed = %d, want 1 — abandonments must NOT inflate the failure count", st.Failed)
	}
	if st.Abandoned != 3 {
		t.Errorf("abandoned = %d, want 3", st.Abandoned)
	}
	if st.Scenarios[scenarioAbandonedHandback] != 2 || st.Scenarios[scenarioAbandonedDisconnect] != 1 {
		t.Errorf("abandon scenarios wrong: %+v", st.Scenarios)
	}
	// Only the completed and failed runs are in the pool: sorted [100 200],
	// len/2 = index 1. Were the 1560s hand-back counted, p50 would be 200 with
	// a pool of [100 200 600 1560] -> index 2 = 600.
	if st.DurationP50S != 200 {
		t.Errorf("p50 = %v, want 200 — an abandonment's wait is not a run duration", st.DurationP50S)
	}
	// The sentinel-compliance ratchet reads completed only, so it is untouched.
	if st.ChromeIdleShare != 0 {
		t.Errorf("chrome_idle_share = %v, want 0", st.ChromeIdleShare)
	}
}

func TestReadTaskRunsForUser(t *testing.T) {
	path := scratchRunLog(t)
	now := time.Now().UTC()
	mk := func(age time.Duration, user, task, outcome, reason string) string {
		rec := TaskRunRecord{
			TS:       now.Add(-age).Format(time.RFC3339),
			TaskID:   task,
			Username: user,
			Backend:  "litellm",
			Outcome:  outcome,
			Reason:   reason,
		}
		b, _ := json.Marshal(rec)
		return string(b)
	}
	lines := []string{
		mk(3*time.Hour, "alice", "t-old", outcomeFailed, "env broke"),
		mk(2*time.Hour, "bob", "t-bob", outcomeFailed, "not alice's"),
		mk(time.Hour, "alice", "t-new", outcomeAbandoned, "abandoned: connection lost with the task still held"),
		mk(48*time.Hour, "alice", "t-window", outcomeFailed, "outside the window"),
		`{"torn json`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runs, err := readTaskRunsForUser(path, "alice", 24*time.Hour, 100)
	if err != nil {
		t.Fatalf("readTaskRunsForUser: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2 (bob excluded, out-of-window excluded): %+v", len(runs), runs)
	}
	// Newest first: an operator opening this asks "what just happened".
	if runs[0].TaskID != "t-new" || runs[1].TaskID != "t-old" {
		t.Errorf("order = %q,%q, want t-new,t-old", runs[0].TaskID, runs[1].TaskID)
	}
	// The reason is the whole point of the endpoint.
	if !strings.Contains(runs[0].Reason, "connection lost") {
		t.Errorf("reason lost: %q", runs[0].Reason)
	}

	// GitHub logins are case-insensitive; an operator pasting one out of the
	// activity rail must not get an empty answer over capitalization.
	if mixed, err := readTaskRunsForUser(path, "ALICE", 24*time.Hour, 100); err != nil || len(mixed) != 2 {
		t.Errorf("case-insensitive match failed: %d runs, err %v", len(mixed), err)
	}

	// limit truncates AFTER the newest-first sort, so it keeps the newest.
	one, err := readTaskRunsForUser(path, "alice", 24*time.Hour, 1)
	if err != nil || len(one) != 1 || one[0].TaskID != "t-new" {
		t.Errorf("limit=1 gave %+v, want just t-new", one)
	}

	// No username: every user's runs, which is what the endpoint returns when
	// the parameter is omitted.
	if all, err := readTaskRunsForUser(path, "", 24*time.Hour, 100); err != nil || len(all) != 3 {
		t.Errorf("unfiltered read gave %d runs, want 3", len(all))
	}

	// A missing file is an empty list, not an error — same contract as stats.
	if r, err := readTaskRunsForUser(path+".missing", "alice", 0, 100); err != nil || len(r) != 0 {
		t.Errorf("missing file must read empty: %+v %v", r, err)
	}
}

// appendAbandonedRun's duration branch, both ways. A task adopted on the
// resume path has no fresh assignment timestamp, and reporting that as a
// 0-second run would read as an instant hand-back — the very thing an operator
// is trying to tell apart from a 26-minute stall. omitempty then keeps the
// field off the record entirely, which is the honest answer: unknown.
func TestAppendAbandonedRun_DurationOnlyWhenKnown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assignedAt time.Time
		wantPos    bool
	}{
		{"assigned by this hub", time.Now().Add(-90 * time.Second), true},
		{"adopted on resume, no assignment timestamp", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := scratchRunLog(t)
			var hub *ContributeWSHub // nil hub tolerated, per the best-effort contract
			conn := &ContributorConnection{
				profile:    &ContributorProfile{GitHubUsername: "dur-user"},
				cliBackend: "litellm",
				model:      "Qwen/Qwen3.6-35B-A3B",
				role:       "contributor",
			}
			hub.appendAbandonedRun(conn, &WSTaskAssign{TaskID: "t-1", Repo: "myorg/repo1", Number: 7},
				abandonCauseDisconnect, tc.assignedAt)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read run log: %v", err)
			}
			var rec TaskRunRecord
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if rec.Outcome != outcomeAbandoned || rec.Scenario != scenarioAbandonedDisconnect {
				t.Errorf("outcome/scenario = %q/%q", rec.Outcome, rec.Scenario)
			}
			if tc.wantPos && rec.DurationS < 90 {
				t.Errorf("duration_s = %v, want >= 90", rec.DurationS)
			}
			if !tc.wantPos {
				if rec.DurationS != 0 {
					t.Errorf("duration_s = %v, want unset for an adopted task", rec.DurationS)
				}
				if strings.Contains(string(data), "duration_s") {
					t.Errorf("unknown duration must be omitted, not serialized as 0: %s", data)
				}
			}
		})
	}
}

// A nil task or an unregistered connection must not panic the release path —
// telemetry is best-effort and runs inside the same defer that tears a socket
// down.
func TestAppendAbandonedRun_ToleratesMissingInputs(t *testing.T) {
	path := scratchRunLog(t)
	var hub *ContributeWSHub
	hub.appendAbandonedRun(nil, &WSTaskAssign{TaskID: "t"}, abandonCauseHandback, time.Now())
	hub.appendAbandonedRun(&ContributorConnection{}, &WSTaskAssign{TaskID: "t"}, abandonCauseHandback, time.Now())
	hub.appendAbandonedRun(&ContributorConnection{profile: &ContributorProfile{GitHubUsername: "u"}}, nil,
		abandonCauseHandback, time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no record should have been written, stat err = %v", err)
	}
}

func TestHandleContributeRuns(t *testing.T) {
	path := scratchRunLog(t)
	rec := TaskRunRecord{
		TS:           time.Now().UTC().Format(time.RFC3339),
		TaskID:       "ct-9",
		Username:     "alice",
		Backend:      "litellm",
		Outcome:      outcomeAbandoned,
		AbandonCause: abandonCauseHandback,
		Reason:       abandonReason(abandonCauseHandback),
		Scenario:     scenarioAbandonedHandback,
		DurationS:    1560,
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleContributeRuns(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=alice", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Username   string          `json:"username"`
		WindowDays int             `json:"window_days"`
		Returned   int             `json:"returned"`
		Runs       []TaskRunRecord `json:"runs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if body.Username != "alice" || body.Returned != 1 || len(body.Runs) != 1 {
		t.Fatalf("body = %+v", body)
	}
	got := body.Runs[0]
	// Every field the operator in #7317 could not reach from the dashboard.
	if got.Outcome != outcomeAbandoned || got.AbandonCause != abandonCauseHandback {
		t.Errorf("outcome/cause = %q/%q", got.Outcome, got.AbandonCause)
	}
	if got.Scenario != scenarioAbandonedHandback {
		t.Errorf("scenario = %q", got.Scenario)
	}
	if got.DurationS != 1560 {
		t.Errorf("duration = %v, want 1560", got.DurationS)
	}
	if !strings.Contains(got.Reason, "asked for new work") {
		t.Errorf("reason = %q", got.Reason)
	}

	// A username nobody matches is an empty list and a 200 — "this contributor
	// has no runs" is an answer, not an error.
	rr = httptest.NewRecorder()
	s.handleContributeRuns(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=nobody", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"returned":0`) {
		t.Errorf("unknown user = %d %s", rr.Code, rr.Body.String())
	}

	// An out-of-range limit falls back to the default rather than 400ing or
	// letting a caller ask for the whole file.
	rr = httptest.NewRecorder()
	s.handleContributeRuns(rr, httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=alice&limit=99999", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"limit":100`) {
		t.Errorf("out-of-range limit = %d %s", rr.Code, rr.Body.String())
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

// #7317 item 2, end to end through the REAL `ready`-while-holding path.
//
// This is the shape of the session that prompted the issue: a contributor picks
// a task up and asks for new work without ever reporting on it. Before this
// change the hub booked the cooldown, wrote "released: gave the task back" to
// the capped activity rail, and recorded NOTHING durable — so eleven hand-backs
// in two hours left one row in the run log and one failure in run-stats. The
// assertion that matters is that the abandonment is now readable per-run, by
// username, after the fact.
func TestTaskRunLog_RecordedOnHandback(t *testing.T) {
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
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "litellm", Model: "Qwen/Qwen3.6-35B-A3B"})
	readMsg(t, conn) // auth_ok

	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					noWorkIssue(92, "handback issue", time.Now().Add(-24*time.Hour)),
					noWorkIssue(93, "second issue", time.Now().Add(-24*time.Hour)),
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
	// The hand-back: ready again, still holding the task, no terminal report.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})

	// The handler writes the record asynchronously relative to this goroutine,
	// and appendTaskRun O_CREATEs the file before writing, so a fixed sleep can
	// observe an empty or absent file on a loaded runner (#6453). Polled via
	// testutil.EventuallyValue rather than a sleep loop — the sleep ratchet in
	// internal/testutil counts a new time.Sleep in a test as a regression, and
	// this is exactly the wait it points at Eventually for.
	data := testutil.EventuallyValue(t, 5*time.Second, func() ([]byte, bool) {
		b, err := os.ReadFile(path)
		return b, err == nil && bytes.Contains(b, []byte("\n"))
	}, "no abandonment record written to %s", path)

	var rec TaskRunRecord
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(data)), "\n")[0]), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Username != "handback-user" || rec.Backend != "litellm" {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.Outcome != outcomeAbandoned || rec.AbandonCause != abandonCauseHandback {
		t.Errorf("outcome/cause = %q/%q, want abandoned/handback", rec.Outcome, rec.AbandonCause)
	}
	if rec.Scenario != scenarioAbandonedHandback {
		t.Errorf("scenario = %q, want %q", rec.Scenario, scenarioAbandonedHandback)
	}
	if !strings.Contains(rec.Reason, "asked for new work") {
		t.Errorf("reason = %q — the synthetic reason is what an operator reads", rec.Reason)
	}
	if rec.Repo != "myorg/repo1" || rec.Number != 92 {
		t.Errorf("task identity wrong: %+v", rec)
	}
	// The duration is the most diagnostic field on an abandonment — on the
	// #7317 session it is what identifies the relay timeout that fired (~26 min
	// pane stall vs ~10 min CLI-ready). A record without it cannot answer the
	// question the endpoint exists for.
	if rec.DurationS <= 0 {
		t.Errorf("duration_s = %v, want positive (assignment→hand-back)", rec.DurationS)
	}

	// And it is reachable the way an operator would reach it: by username, on
	// the new endpoint, which is the whole point of #7317.
	req2 := httptest.NewRequest(http.MethodGet, "/api/contribute/runs?username=handback-user&days=1", nil)
	w2 := httptest.NewRecorder()
	s.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("runs endpoint status %d", w2.Code)
	}
	var resp struct {
		Returned int             `json:"returned"`
		Runs     []TaskRunRecord `json:"runs"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("runs response: %v", err)
	}
	if resp.Returned < 1 || resp.Runs[0].AbandonCause != abandonCauseHandback {
		t.Fatalf("endpoint did not serve the abandonment: %+v", resp)
	}

	// run-stats counts it as an abandonment, NOT as a failure — the number the
	// backend ratchet reads must not move because this row started existing.
	req3 := httptest.NewRequest(http.MethodGet, "/api/contribute/run-stats?days=1", nil)
	w3 := httptest.NewRecorder()
	s.mux.ServeHTTP(w3, req3)
	var stats struct {
		Backends []taskRunBackendStats `json:"backends"`
	}
	if err := json.Unmarshal(w3.Body.Bytes(), &stats); err != nil {
		t.Fatalf("run-stats response: %v", err)
	}
	if len(stats.Backends) != 1 || stats.Backends[0].Abandoned != 1 || stats.Backends[0].Failed != 0 {
		t.Fatalf("run-stats wrong: %+v", stats.Backends)
	}
}

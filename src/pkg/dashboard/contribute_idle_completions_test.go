package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A relay that dropped an issue used to leave `verdict=idle verdict_reason=""`
// and nothing else: the withheld list said "Completion cooldown until …", the
// issue was offered again and dropped again. These tests pin that a no-PR
// idle always has a reason on record, that the reason is shown where the
// cooldown is, and that it goes away with the completion it described.

const idleRepo = "acme/widgets"

func idleKey(number int) string { return idleRepo + "#" + strconv.Itoa(number) }

func idleIssue(number int) map[string]any {
	return map[string]any{
		"number": float64(number),
		"title":  "widget work",
		"url":    "https://github.com/" + idleRepo + "/issues/" + strconv.Itoa(number),
		"author": "bob",
	}
}

func idleStatus(s *Server, issues ...map[string]any) {
	list := make([]any, len(issues))
	for i, issue := range issues {
		list[i] = issue
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "widgets", Full: idleRepo, ActionableIssues: list}}}
	s.statusMu.Unlock()
}

func idleRecord(t *testing.T, hub *ContributeWSHub, key string) idleCompletionRecord {
	t.Helper()
	rec, ok := hub.idleCompletionKey(key)
	if !ok {
		t.Fatalf("no idle reason on record for %s", key)
	}
	return rec
}

// An empty reason is replaced by what the completion signal says, a supplied
// one is kept on one line, capped, and with tokens masked.
func TestIdleCompletionReasonIsNeverEmpty(t *testing.T) {
	for _, tc := range []struct{ name, reported, signal, want string }{
		{"chrome idle", "", completionSignalChromeIdle, "agent exited without a conclusion"},
		{"chrome idle, blank reason", " \n\t ", "CHROME_IDLE", "agent exited without a conclusion"},
		{"bare complete", "", completionSignalVerdict, "agent reported complete without a pull request and gave no reason"},
		{"no signal", "", "", "no reason given"},
		{"unrecognised signal", "", "made-up", "no reason given"},
		{"relay reason kept", " agent reported complete without a pull request: nothing to change ", completionSignalVerdict,
			"agent reported complete without a pull request: nothing to change"},
		{"one line", "first line\nsecond\tline", completionSignalVerdict, "first line second line"},
	} {
		if got := normalizeIdleCompletionReason(tc.reported, tc.signal); got != tc.want {
			t.Errorf("%s: normalizeIdleCompletionReason(%q, %q) = %q, want %q", tc.name, tc.reported, tc.signal, got, tc.want)
		}
	}

	long := normalizeIdleCompletionReason(strings.Repeat("word ", 200), completionSignalVerdict)
	if len(long) > noWorkReasonMaxLen || long == "" || strings.HasSuffix(long, " ") {
		t.Errorf("a long reason must be capped at %d bytes with no trailing space, got %d bytes: %q", noWorkReasonMaxLen, len(long), long)
	}

	token := "ghp_" + strings.Repeat("a1B2", 9)
	if got := normalizeIdleCompletionReason("push failed with "+token, completionSignalVerdict); strings.Contains(got, token) || got == "" {
		t.Errorf("a token in the reason must be masked, got %q", got)
	}
}

// Each ending is booked with its reason, the relay's own sentence wins over
// the hub's wording, and an evidence-less idle still books only the flat base
// cooldown however often it repeats (#6723).
func TestIdleCompletionBookedWithReasonBySignal(t *testing.T) {
	hub := ledgerHub(t.TempDir())

	for i, tc := range []struct{ reason, signal, want, wantSignal string }{
		{"", completionSignalChromeIdle, idleCompletionNoConclusion, completionSignalChromeIdle},
		{"", completionSignalVerdict, idleCompletionCompleteNoReason, completionSignalVerdict},
		{"", "", idleCompletionNotGiven, completionSignalUnknown},
		{"agent reported complete without a pull request: the fix belongs in another module", completionSignalVerdict,
			"agent reported complete without a pull request: the fix belongs in another module", completionSignalVerdict},
	} {
		key := idleKey(100 + i)
		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", tc.reason, tc.signal)
		rec := idleRecord(t, hub, key)
		if rec.Reason != tc.want || rec.Signal != tc.wantSignal || rec.Reporter != "alice" || rec.RecordedAt.IsZero() {
			t.Errorf("signal %q reason %q booked %+v, want reason %q signal %q by alice", tc.signal, tc.reason, rec, tc.want, tc.wantSignal)
		}
	}

	// #6723 stays pinned: the reason is explanation, not escalation.
	key := idleKey(200)
	for range 3 {
		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", "", completionSignalChromeIdle)
	}
	if got, want := recordedCooldown(t, hub, key), completedNoPRCooldownHours*time.Hour; got != want {
		t.Fatalf("an evidence-less idle booked %v after three in a row, want the flat base cooldown %v", got, want)
	}
	hub.completedMu.Lock()
	_, streak := hub.noPRStreaks[key]
	hub.completedMu.Unlock()
	if streak {
		t.Fatal("an evidence-less idle must not start a no-PR streak")
	}
}

// The completion cooldown row says why the relay dropped the issue; a verified
// PR, or a no-work verdict (which keeps its own reason), takes the note away.
func TestIdleCompletionWithheldDetailShowsLastReason(t *testing.T) {
	hub, s := covK2Hub(t)
	idleStatus(s, idleIssue(7), idleIssue(8))
	key := idleKey(7)

	detail := func() string {
		t.Helper()
		item, ok := findWithheld(hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll).withheld, key)
		if !ok {
			t.Fatalf("%s is not in the withheld list", key)
		}
		if item.Reason != withheldReasonCooldown || item.CooldownUntil == "" {
			t.Fatalf("withheld row = %+v, want a completion cooldown with its expiry", item)
		}
		return item.Detail
	}

	const reason = "agent reported complete without a pull request: the fix belongs in another module"
	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", reason, completionSignalVerdict)
	got := detail()
	if !strings.HasPrefix(got, "Completion cooldown until ") || !strings.HasSuffix(got, " — last relay: "+reason) {
		t.Fatalf("detail = %q, want \"Completion cooldown until <time> — last relay: %s\"", got, reason)
	}

	// A relay that gave no reason still leaves one.
	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", "", completionSignalChromeIdle)
	if got := detail(); !strings.HasSuffix(got, " — last relay: agent exited without a conclusion") {
		t.Fatalf("detail = %q, want the chrome-idle wording", got)
	}

	// A no-work verdict is not an idle: its reason lives in the verdict ledger.
	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictNoWorkNeeded, "alice", "already covered", completionSignalVerdict)
	if got := detail(); strings.Contains(got, "last relay") {
		t.Fatalf("detail = %q after a no_work_needed verdict, want no idle note", got)
	}

	hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", reason, completionSignalVerdict)
	hub.markTaskCompletedVerdictKeySignal(key, "https://github.com/acme/widgets/pull/9", completionVerdictShipped, "alice", "", completionSignalVerdict)
	if got := detail(); strings.Contains(got, "last relay") {
		t.Fatalf("detail = %q after a verified PR, want no idle note", got)
	}
	if _, ok := hub.idleCompletionKey(key); ok {
		t.Fatal("a verified PR must clear the idle reason")
	}

	// A cooldown with no idle on record reads as it always did.
	hub.completedMu.Lock()
	hub.completedTasks[idleKey(8)] = time.Now()
	hub.completedMu.Unlock()
	other, ok := findWithheld(hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll).withheld, idleKey(8))
	if !ok || strings.Contains(other.Detail, "last relay") || !strings.HasPrefix(other.Detail, "Completion cooldown") {
		t.Fatalf("a cooldown with no idle reason = %+v (found %v), want the plain detail", other, ok)
	}
}

// The ledger is read back after a restart, and a reason past its retention
// is not.
func TestIdleCompletionSurvivesRestart(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())

	hub1, _ := covK2Hub(t)
	hub1.markTaskCompletedVerdictKeySignal(idleKey(21), "", completionVerdictIdle, "alice",
		"agent reported complete without a pull request: nothing to change", completionSignalVerdict)
	hub1.completedMu.Lock()
	hub1.idleCompletions[idleKey(22)] = idleCompletionRecord{
		RecordedAt: time.Now().Add(-idleCompletionRetention - time.Hour),
		Reporter:   "alice",
		Reason:     "stale",
	}
	hub1.completedMu.Unlock()
	hub1.saveIdleCompletions()

	hub2, _ := covK2Hub(t)
	rec := idleRecord(t, hub2, idleKey(21))
	if rec.Reporter != "alice" || rec.Reason != "agent reported complete without a pull request: nothing to change" ||
		rec.Signal != completionSignalVerdict || rec.RecordedAt.IsZero() {
		t.Fatalf("the idle reason did not round-trip: %+v", rec)
	}
	if _, ok := hub2.idleCompletionKey(idleKey(22)); ok {
		t.Fatal("an idle reason past its retention must not be loaded")
	}
}

// A relay written before the verdict, reason and signal fields existed still
// completes, is booked idle on the ordinary no-PR cooldown, and leaves "no
// reason given" in the ledger and in its run record.
func TestIdleCompletionOlderRelayBookedIdleWithNoReasonGiven(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{"github_username":"alice"}`))
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
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	idleStatus(s, idleIssue(31))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	// No verdict, no verdict_reason, no completion_signal.
	conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "completed"})
	// The hub handles one frame at a time, so the answer to this `ready` is
	// written only after the completion above has been booked.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	if next := readMsg(t, conn); next.Type != "task_unavailable" {
		t.Fatalf("the issue is in its completion cooldown and must not be handed back, got %+v", next)
	}

	hub := s.contributeHub
	key := idleKey(31)
	if rec := idleRecord(t, hub, key); rec.Reason != idleCompletionNotGiven || rec.Signal != completionSignalUnknown || rec.Reporter != "alice" {
		t.Fatalf("an older relay's idle booked %+v, want %q with an unknown signal by alice", rec, idleCompletionNotGiven)
	}
	if got, want := recordedCooldown(t, hub, key), completedNoPRCooldownHours*time.Hour; got != want {
		t.Fatalf("an older relay's first idle booked %v, want the ordinary no-PR cooldown %v", got, want)
	}
	runs, err := readTaskRunsForUser(hub.taskRunLogPath(), "alice", time.Hour, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("task-run log: %d records, err %v; want the one completion", len(runs), err)
	}
	if runs[0].Verdict != completionVerdictIdle || runs[0].VerdictReason != idleCompletionNotGiven {
		t.Fatalf("run record verdict %q reason %q, want idle with %q", runs[0].Verdict, runs[0].VerdictReason, idleCompletionNotGiven)
	}
}

// The Activity monitor's issue row carries the same clause the withheld list
// shows, and only while no pull request covers the issue.
func TestStageTrackerLastIdle(t *testing.T) {
	now := time.Now().UTC()
	_, s := covK2Hub(t)
	hub := s.contributeHub
	if hub == nil {
		t.Fatal("the test server has no contribute hub")
	}
	hub.markTaskCompletedVerdictKeySignal(idleKey(12), "", completionVerdictIdle, "alice", "", completionSignalChromeIdle)
	hub.markTaskCompletedVerdictKeySignal(idleKey(13), "", completionVerdictIdle, "alice", "", completionSignalChromeIdle)

	store := stagelabels.NewStore("", func() time.Time { return now })
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "widgets", Full: idleRepo,
		ActionableIssues: []any{
			github.Issue{Number: 12, Title: "Dropped by a relay", Author: "bob", AuthorIsHuman: true, Labels: []string{"stage/010-actionable"}},
			github.Issue{Number: 13, Title: "Has a PR under way", Author: "bob", AuthorIsHuman: true, Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 40, State: "open"}}},
			github.Issue{Number: 14, Title: "Never offered", Author: "bob", AuthorIsHuman: true, Labels: []string{"stage/010-actionable"}},
		},
	}}
	resp := s.buildStageTracker(status, store, now)

	idle := trackerItem(t, resp, "issue", 12).LastIdle
	if idle == nil {
		t.Fatal("issue #12 has an idle reason on record but its row carries no last_idle")
	}
	if idle.Text != "last relay: agent exited without a conclusion" || idle.Reason != idleCompletionNoConclusion ||
		idle.Contributor != "alice" || idle.At == nil {
		t.Fatalf("last_idle = %+v", *idle)
	}
	if got := trackerItem(t, resp, "issue", 13).LastIdle; got != nil {
		t.Fatalf("an issue with a pull request under way must not carry last_idle, got %+v", *got)
	}
	if got := trackerItem(t, resp, "issue", 14).LastIdle; got != nil {
		t.Fatalf("an issue with no idle on record must not carry last_idle, got %+v", *got)
	}

	// The row's tooltip prints the server's clause.
	body := extractJSFunction(t, indexHTML(t), "stageTrackerTooltip")
	if !strings.Contains(body, "item.last_idle.text") {
		t.Error("the Activity monitor tooltip does not show last_idle.text")
	}
}

package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/pkg/config"
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
		// The hub only knows the relay sent nothing: an older relay discards
		// the agent's sentence, so the hub does not say the agent gave none.
		{"bare complete", "", completionSignalVerdict, "agent reported complete without a pull request; the relay passed no reason along"},
		{"bare complete, signal in capitals", "", " VERDICT ", "agent reported complete without a pull request; the relay passed no reason along"},
		// A newer relay whose agent gave no sentence says so itself; that text is the relay's and is kept.
		{"relay says the agent gave none", "agent reported complete without a pull request and gave no reason", completionSignalVerdict,
			"agent reported complete without a pull request and gave no reason"},
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
// is not. The second half writes the file itself: saveIdleCompletions drops
// an expired record before it writes, so a test that only went through save
// would pass with the expiry filter in loadIdleCompletions removed.
func TestIdleCompletionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	hub1 := ledgerHub(dir)
	hub1.markTaskCompletedVerdictKeySignal(idleKey(21), "", completionVerdictIdle, "alice",
		"agent reported complete without a pull request: nothing to change", completionSignalVerdict)

	hub2 := ledgerHub(dir)
	hub2.loadIdleCompletions()
	rec := idleRecord(t, hub2, idleKey(21))
	if rec.Reporter != "alice" || rec.Reason != "agent reported complete without a pull request: nothing to change" ||
		rec.Signal != completionSignalVerdict || rec.RecordedAt.IsZero() {
		t.Fatalf("the idle reason did not round-trip: %+v", rec)
	}

	// A ledger file as an earlier process left it: one live record, one past
	// its retention, one with no reason.
	now := time.Now().UTC()
	onDisk := map[string]idleCompletionRecord{
		idleKey(22): {RecordedAt: now.Add(-time.Hour), Reporter: "alice", Reason: "agent exited without a conclusion", Signal: completionSignalChromeIdle},
		idleKey(23): {RecordedAt: now.Add(-idleCompletionRetention - time.Hour), Reporter: "alice", Reason: "stale"},
		idleKey(24): {RecordedAt: now.Add(-time.Hour), Reporter: "alice"},
	}
	data, err := json.Marshal(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	dir3 := t.TempDir()
	hub3 := ledgerHub(dir3)
	path := filepath.Join(dir3, idleCompletionsFileName)
	if got := hub3.idleCompletionsPath(); got != path {
		t.Fatalf("idleCompletionsPath() = %q, want %q beside the no-PR streak ledger", got, path)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	hub3.loadIdleCompletions()
	// Read the map itself: idleCompletionKey hides an expired record on its
	// own, which would mask a load that kept one.
	hub3.completedMu.Lock()
	loaded := make(map[string]idleCompletionRecord, len(hub3.idleCompletions))
	for k, v := range hub3.idleCompletions {
		loaded[k] = v
	}
	hub3.completedMu.Unlock()
	if got, ok := loaded[idleKey(22)]; !ok || got.Reason != "agent exited without a conclusion" || got.Reporter != "alice" {
		t.Fatalf("the live record was not loaded: %+v (found %v)", got, ok)
	}
	if _, ok := loaded[idleKey(23)]; ok {
		t.Fatal("a record past its retention was loaded from disk")
	}
	if _, ok := loaded[idleKey(24)]; ok {
		t.Fatal("a record with no reason was loaded from disk")
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d records, want only the live one: %+v", len(loaded), loaded)
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

// A completion can be booked idle although the relay reported a pull request:
// the hub could not verify it (a GitHub lookup that failed, another author).
// The reason on record says that, with the verifier's own reason. It must not
// say the agent stopped without a pull request.
func TestIdleCompletionReportedPRReasonSaysWhatTheHubMadeOfIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		verified     bool
		verifyReason string
		want         string
	}{
		{"lookup failed", false, "github lookup failed", "relay reported a pull request the hub could not verify: github lookup failed"},
		{"reason on several lines", false, " author mismatch:\n alice ", "relay reported a pull request the hub could not verify: author mismatch: alice"},
		{"no verifier reason", false, "", "relay reported a pull request the hub could not verify"},
		{"verified, not settled", true, "", "relay reported a pull request the hub verified but could not settle as shipped"},
	} {
		if got := idleCompletionReportedPRReason(tc.verified, tc.verifyReason); got != tc.want {
			t.Errorf("%s: reason %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Over the WebSocket: a relay reports a pull request the hub cannot verify (the
// test hub has no GitHub client) and sends no reason. The completion is booked
// idle, and the ledger and the run record say the pull request was reported
// and not verified.
func TestIdleCompletionWithUnverifiedReportedPRSaysSo(t *testing.T) {
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

	idleStatus(s, idleIssue(32))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	prURL := "https://github.com/" + idleRepo + "/pull/77"
	conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "completed", PRURL: prURL, CompletionSignal: completionSignalVerdict})
	// The hub handles one frame at a time, so the answer to this `ready` is
	// written only after the completion above has been booked.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	if next := readMsg(t, conn); next.Type != "task_unavailable" {
		t.Fatalf("the issue is in its completion cooldown and must not be handed back, got %+v", next)
	}

	hub := s.contributeHub
	runs, err := readTaskRunsForUser(hub.taskRunLogPath(), "alice", time.Hour, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("task-run log: %d records, err %v; want the one completion", len(runs), err)
	}
	run := runs[0]
	if run.Verdict != completionVerdictIdle || run.PRVerified || run.ReportedPRURL != prURL || run.PRVerifyReason == "" {
		t.Fatalf("run record %+v, want an idle with the reported pull request unverified and a verifier reason", run)
	}
	want := idleCompletionReportedPRReason(false, run.PRVerifyReason)
	if !strings.HasPrefix(want, idleCompletionUnverifiedPR+": ") {
		t.Fatalf("expected reason %q does not carry the verifier's reason", want)
	}
	if run.VerdictReason != want {
		t.Fatalf("run record reason %q, want %q", run.VerdictReason, want)
	}
	if rec := idleRecord(t, hub, idleKey(32)); rec.Reason != want || rec.Reporter != "alice" {
		t.Fatalf("ledger booked %+v, want %q by alice", rec, want)
	}
	if strings.Contains(run.VerdictReason, "without a pull request") {
		t.Fatalf("reason %q says the agent opened no pull request, but one was reported", run.VerdictReason)
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

	// The note lives as long as the completion cooldown that idle booked. Once
	// it lapses the row stops carrying it, while the ledger still has the
	// reason.
	hub.completedMu.Lock()
	hub.completedTasks[idleKey(12)] = time.Now().Add(-(hub.completedTaskCooldown[idleKey(12)] + time.Minute))
	hub.completedMu.Unlock()
	resp = s.buildStageTracker(status, store, now)
	if got := trackerItem(t, resp, "issue", 12).LastIdle; got != nil {
		t.Fatalf("the completion cooldown has lapsed; the row must not carry last_idle, got %+v", *got)
	}
	if _, ok := hub.idleCompletionKey(idleKey(12)); !ok {
		t.Fatal("a lapsed cooldown hides the note on the row; it must not remove the reason from the ledger")
	}

	// The row's tooltip prints the server's clause.
	body := extractJSFunction(t, indexHTML(t), "stageTrackerTooltip")
	if !strings.Contains(body, "item.last_idle.text") {
		t.Error("the Activity monitor tooltip does not show last_idle.text")
	}
}

// The Activity monitor note follows the completion cooldown, the gate the
// withheld list's cooldown row is shown under: live cooldown, note; cooldown
// switched off by the operator, no note; lapsed cooldown, no note. And a task
// that later fails or is let go on the issue removes the reason for good, on
// both surfaces, because that idle is no longer what the last relay did.
func TestStageTrackerLastIdleFollowsTheCompletionCooldown(t *testing.T) {
	cfg := &config.Config{}
	hub := ledgerHub(t.TempDir())
	hub.server = &Server{deps: &Dependencies{Config: cfg}}
	srv := &Server{contributeHub: hub}
	key := idleKey(41)
	book := func() {
		hub.markTaskCompletedVerdictKeySignal(key, "", completionVerdictIdle, "alice", "", completionSignalChromeIdle)
	}

	book()
	if idle := srv.stageTrackerLastIdle(idleRepo, 41); idle == nil || idle.Text != "last relay: agent exited without a conclusion" {
		t.Fatalf("inside the completion cooldown the row must carry the note, got %+v", idle)
	}

	// Cooldown switched off: the gate answers false, so no note is shown,
	// and nothing is lost: switching it back on shows it again.
	off, on := false, true
	cfg.Hub.ContributeCooldownEnabled = &off
	if idle := srv.stageTrackerLastIdle(idleRepo, 41); idle != nil {
		t.Fatalf("with the completion cooldown switched off the row must carry no note, got %+v", *idle)
	}
	cfg.Hub.ContributeCooldownEnabled = &on
	if idle := srv.stageTrackerLastIdle(idleRepo, 41); idle == nil {
		t.Fatal("switching the cooldown back on inside its window must show the note again")
	}

	// A later task on the issue failed. The completion cooldown is still
	// running (a failure does not touch it), but the idle is not the last
	// thing a relay did, so the reason is gone from the ledger.
	hub.recordTaskFailureKey(key, false)
	if !hub.isTaskInCooldownKey(key) {
		t.Fatal("setup: a failure must leave the completion cooldown in force")
	}
	if _, ok := hub.idleCompletionKey(key); ok {
		t.Fatal("a later failure on the issue must remove the idle reason")
	}
	if idle := srv.stageTrackerLastIdle(idleRepo, 41); idle != nil {
		t.Fatalf("after a later failure the row must carry no note, got %+v", *idle)
	}
	reloaded := ledgerHub(filepath.Dir(hub.idleCompletionsPath()))
	reloaded.loadIdleCompletions()
	if _, ok := reloaded.idleCompletionKey(key); ok {
		t.Fatal("the removal must reach the ledger file, or a restart brings the stale reason back")
	}

	// The same for a task let go on a dropped connection.
	book()
	hub.bookReleaseCooldownKey(key)
	if _, ok := hub.idleCompletionKey(key); ok {
		t.Fatal("a later release on the issue must remove the idle reason")
	}

	// Lapsed cooldown: no note, and the ledger keeps the reason.
	book()
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-(hub.completedTaskCooldown[key] + time.Minute))
	hub.completedMu.Unlock()
	if idle := srv.stageTrackerLastIdle(idleRepo, 41); idle != nil {
		t.Fatalf("after the completion cooldown lapsed the row must carry no note, got %+v", *idle)
	}
	if _, ok := hub.idleCompletionKey(key); !ok {
		t.Fatal("a lapsed cooldown must not remove the reason from the ledger")
	}
}

// Over the WebSocket: a blocked verdict that names nothing it waits on is
// demoted to idle by the hub. The agent said blocked, not complete, so the
// ledger and the run record say that, whether the reason was absent or was
// only a footer line.
func TestIdleCompletionReasonlessBlockedVerdictSaysBlocked(t *testing.T) {
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

	hub := s.contributeHub
	fx := newToolingReleaseFixture()
	hub.toolingBlockedReleaser = fx.release

	idleStatus(s, idleIssue(33), idleIssue(34))
	// The first completion carries no reason at all, the second only a footer.
	reasons := []string{"", "---"}
	var numbers []int
	for i, reason := range reasons {
		conn.WriteJSON(WSMessage{Type: "ready", Seq: 2 + i})
		assign := readMsg(t, conn)
		if assign.Type != "task_assign" {
			t.Fatalf("completion %d: expected task_assign, got %+v", i+1, assign)
		}
		numbers = append(numbers, assign.Number)
		conn.WriteJSON(WSMessage{
			Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
			CompletionSignal: completionSignalVerdict,
			Verdict:          "no_work_needed", VerdictBlocked: true,
			VerdictReason: reason,
		})
	}
	// The hub handles one frame at a time, so the answer to this `ready` is
	// written only after both completions have been booked.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 9})
	if next := readMsg(t, conn); next.Type != "task_unavailable" {
		t.Fatalf("both issues are in their completion cooldown, got %+v", next)
	}
	for range reasons {
		select {
		case <-fx.released:
		case <-time.After(5 * time.Second):
			t.Fatal("the hub must release the blocked label through the seam for each demoted verdict")
		}
	}

	for _, number := range numbers {
		rec := idleRecord(t, hub, idleKey(number))
		if rec.Reason != "agent reported blocked without saying what it waits on" || rec.Reporter != "alice" {
			t.Fatalf("#%d: a reasonless blocked verdict booked %+v, want %q", number, rec, idleCompletionBlockedNoReason)
		}
		hub.completedMu.Lock()
		_, blockedRow := hub.noWorkVerdicts[idleKey(number)]
		hub.completedMu.Unlock()
		if blockedRow {
			t.Fatalf("#%d: a demoted blocked verdict must not leave a blocked ledger row", number)
		}
	}
	runs, err := readTaskRunsForUser(hub.taskRunLogPath(), "alice", time.Hour, 10)
	if err != nil || len(runs) != len(reasons) {
		t.Fatalf("task-run log: %d records, err %v; want the %d completions", len(runs), err, len(reasons))
	}
	for _, run := range runs {
		if run.Verdict != completionVerdictIdle || run.VerdictReason != idleCompletionBlockedNoReason {
			t.Fatalf("run record for #%d: verdict %q reason %q, want idle with %q", run.Number, run.Verdict, run.VerdictReason, idleCompletionBlockedNoReason)
		}
	}
}

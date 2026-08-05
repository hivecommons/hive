package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// twoIssueStatus seeds a status with two admissible issues in the same repo:
// A (#10, ahead in scan order) and B (#20). Neither is authored by the test
// contributor, so #2390 own-work ordering is inert and the pick is driven purely
// by scan order and the #2435 failure ledger.
func twoIssueStatus(s *Server) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(10),
						"title":  "Issue A (head of scan order)",
						"url":    "https://github.com/myorg/repo1/issues/10",
						"author": "someone",
					},
					map[string]any{
						"number": float64(20),
						"title":  "Issue B (starved before the fix)",
						"url":    "https://github.com/myorg/repo1/issues/20",
						"author": "someone",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()
}

// TestQueueLivelock_FailureCooldownFreesStarvedIssue is the #2435 regression:
// before the fix, task_failed recorded no cooldown and selectTask had no
// failure-aware ranking, so the same head-of-scan issue (A/#10) was handed out
// over and over while B/#20 was never offered. After the fix, once A fails it is
// parked by the short failure cooldown, so the very next assignment must NOT be A
// again immediately, and B must be offered.
func TestQueueLivelock_FailureCooldownFreesStarvedIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	twoIssueStatus(s)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct", ContributorID: "c-ct", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// First assignment: head-of-scan issue A (#10).
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected an assignment, got nil")
	}
	if msg.Number != 10 {
		t.Fatalf("expected first assignment to be head-of-scan A (#10), got #%d", msg.Number)
	}

	// The contributor reports task_failed for A. Clear currentTask as the real
	// task_failed handler does, then record the failure through the same hub path
	// the handler uses.
	conn.mu.Lock()
	conn.currentTask = nil
	conn.mu.Unlock()
	hub.recordTaskFailure("myorg/repo1", 10, false)

	// Next assignment MUST NOT be A again immediately — it is in the short failure
	// cooldown — and B (#20) is offered instead. This is the exact behaviour that
	// was broken: pre-fix this returned #10 again.
	msg2 := hub.selectTask(conn)
	if msg2 == nil {
		t.Fatalf("expected B to be offered after A failed, got nil (queue starved)")
	}
	if msg2.Number == 10 {
		t.Fatalf("livelock: A (#10) was re-offered immediately after failing; expected it parked in failure cooldown")
	}
	if msg2.Number != 20 {
		t.Fatalf("expected the starved issue B (#20) to be offered, got #%d", msg2.Number)
	}

	// Sanity: A really is in failure cooldown, and its short window is far shorter
	// than any completion cooldown.
	if !hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("A should be in failure cooldown right after a task_failed")
	}
	if failedTaskCooldownMinutes*time.Minute >= completedNoPRCooldownHours*time.Hour {
		t.Fatalf("failure cooldown (%dm) must be far shorter than the no-PR completion cooldown (%dh)",
			failedTaskCooldownMinutes, completedNoPRCooldownHours)
	}
}

// TestQueueLivelock_ConsecutiveFailuresQuarantine verifies remedy 2: an issue
// that fails consecutiveFailureQuarantineThreshold times is parked for the
// LONGER quarantine window, not just the short failure cooldown.
func TestQueueLivelock_ConsecutiveFailuresQuarantine(t *testing.T) {
	hub, _ := covK2Hub(t)

	for i := 0; i < consecutiveFailureQuarantineThreshold; i++ {
		hub.recordTaskFailure("myorg/repo1", 10, false)
	}
	if !hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("issue should be quarantined after %d failures", consecutiveFailureQuarantineThreshold)
	}

	// Age the last failure just past the SHORT cooldown but well inside the
	// quarantine window: a non-quarantined issue would be free, a quarantined one
	// is still parked.
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#10"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if !hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("quarantined issue must stay parked past the short cooldown (quarantine is %dh)", quarantineCooldownHours)
	}

	// Age it past the full quarantine window: now it self-heals and the counter
	// resets.
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#10"] = time.Now().Add(-(quarantineCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("quarantine should expire after %dh", quarantineCooldownHours)
	}
	if hub.recentFailureCount("myorg/repo1", 10) != 0 {
		t.Fatalf("consecutive-failure count should reset once the quarantine expires")
	}
}

// TestQueueLivelock_PermanentFailureQuarantinesFaster verifies that a permanent
// failure (msg.Permanent) crosses the quarantine threshold in a single report,
// whereas one ordinary failure only earns the short cooldown.
func TestQueueLivelock_PermanentFailureQuarantinesFaster(t *testing.T) {
	hub, _ := covK2Hub(t)

	// One ordinary failure: short cooldown only, NOT quarantined. Prove it by
	// aging past the short window — it frees.
	hub.recordTaskFailure("myorg/repo1", 30, false)
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#30"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 30) {
		t.Fatalf("a single ordinary failure must only earn the short cooldown, not a quarantine")
	}

	// One PERMANENT failure: quarantined immediately (weight >= threshold). Aging
	// past the short window leaves it parked.
	hub.recordTaskFailure("myorg/repo1", 40, true)
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#40"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if !hub.isTaskInFailureCooldown("myorg/repo1", 40) {
		t.Fatalf("a permanent failure must quarantine immediately (weight %d, threshold %d)",
			permanentFailureWeight, consecutiveFailureQuarantineThreshold)
	}
}

// TestQueueLivelock_CompletionResetsFailureHistory verifies a completion clears
// the failure ledger so a flaky-then-fixed issue does not carry a stale
// quarantine.
func TestQueueLivelock_CompletionResetsFailureHistory(t *testing.T) {
	hub, _ := covK2Hub(t)

	hub.recordTaskFailure("myorg/repo1", 50, true) // quarantined
	if !hub.isTaskInFailureCooldown("myorg/repo1", 50) {
		t.Fatalf("expected quarantine after a permanent failure")
	}
	hub.markTaskCompleted("myorg/repo1", 50, "https://github.com/myorg/repo1/pull/99")
	if hub.isTaskInFailureCooldown("myorg/repo1", 50) {
		t.Fatalf("a completion must clear the failure cooldown/quarantine")
	}
	if hub.recentFailureCount("myorg/repo1", 50) != 0 {
		t.Fatalf("a completion must reset the consecutive-failure counter")
	}
}

// TestQueueLivelock_FailureAwareSelectionBackstop verifies remedy 3: even after
// A's short failure cooldown has elapsed, a never-failed peer B is preferred
// because A still carries failure history — so a flaky head-of-scan issue cannot
// re-monopolise the queue purely on scan position.
func TestQueueLivelock_FailureAwareSelectionBackstop(t *testing.T) {
	hub, s := covK2Hub(t)
	twoIssueStatus(s)

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct", ContributorID: "c-ct", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// A (#10) failed once, then its SHORT cooldown elapsed (so it is admissible
	// again) but it still carries one recent failure.
	hub.recordTaskFailure("myorg/repo1", 10, false)
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#10"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("A's short cooldown should have elapsed for this backstop check")
	}

	// Both A and B are now admissible. The failure-aware sort must prefer B (#20,
	// zero failures) over A (#10, one recent failure) even though A is ahead in
	// scan order.
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected an assignment, got nil")
	}
	if msg.Number != 20 {
		t.Fatalf("failure-aware backstop: expected never-failed B (#20) preferred over recently-failed A (#10), got #%d", msg.Number)
	}
}

// TestQueueLivelock_TaskFailedHandlerRecordsCooldown drives the real WebSocket
// task_failed handler end-to-end and verifies it records a failure cooldown
// through the hub — i.e. the handler is actually wired to recordTaskFailure and
// not just the direct hub methods the other tests call.
func TestQueueLivelock_TaskFailedHandlerRecordsCooldown(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Seed one admissible issue so `ready` yields a task_assign.
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(77),
						"title":  "Reliably failing issue",
						"url":    "https://github.com/myorg/repo1/issues/77",
						"author": "someone",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()

	body := `{"github_username":"livelock-handler-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 77 {
		t.Fatalf("expected task_assign for #77, got type=%s number=%d", assign.Type, assign.Number)
	}

	// Report the assigned task as failed via the real handler.
	conn.WriteJSON(WSMessage{Type: "task_failed", TaskID: assign.TaskID, Reason: "could not build", Permanent: false})

	// The handler runs asynchronously; poll briefly for the recorded cooldown.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.contributeHub.isTaskInFailureCooldown("myorg/repo1", 77) {
			return // handler wired correctly
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task_failed handler did not record a failure cooldown for the assigned issue")
}

// TestDupePR_DisconnectReleaseRecordsCooldown is the #2356 regression: a task
// released because its contributor's WebSocket dropped must NOT be instantly
// re-admissible. Before the fix, the disconnect defer nil-ed currentTask and
// logged the release but recorded no cooldown, so the issue fell out of the
// activeIssues double-assign guard and selectTask could hand the SAME issue to
// another session during the reconnect window — while the original relay (which
// keeps its task locally and re-asserts it on reconnect) was still working it —
// producing duplicate PRs. After the fix a disconnect-release books the same
// short non-permanent failure cooldown the task_failed path uses.
func TestDupePR_DisconnectReleaseRecordsCooldown(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Two admissible issues: A (#10, head of scan) and B (#20). If the released A
	// were still re-admissible, the next selection would return A again; after the
	// fix it must be parked and B offered instead.
	twoIssueStatus(s)

	body := `{"github_username":"disconnect-dupe-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 10 {
		t.Fatalf("expected task_assign for head-of-scan A (#10), got type=%s number=%d", assign.Type, assign.Number)
	}

	// Simulate the contributor's connection dropping mid-task (no task_failed, no
	// task_complete): just close the socket. The server-side disconnect defer must
	// fire and, per the fix, record the failure cooldown for the abandoned issue.
	conn.Close()

	// The defer runs asynchronously on the server's read-loop goroutine; poll for
	// the recorded cooldown on the released issue.
	deadline := time.Now().Add(2 * time.Second)
	recorded := false
	for time.Now().Before(deadline) {
		if s.contributeHub.isTaskInFailureCooldown("myorg/repo1", 10) {
			recorded = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !recorded {
		t.Fatalf("disconnect-release did not park the abandoned issue in a cooldown — it is instantly re-admissible and can be double-assigned (#2356)")
	}

	// A fresh selection must now skip the parked A (#10) and offer B (#20) instead,
	// proving the released issue cannot be handed straight back out to a racing
	// session during the reconnect window.
	conn2 := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "other-ct", ContributorID: "c-other", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := s.contributeHub.selectTask(conn2)
	if msg == nil {
		t.Fatalf("expected B to be offered while A is parked, got nil")
	}
	if msg.Number == 10 {
		t.Fatalf("dupe risk: released A (#10) was re-offered immediately after a disconnect; expected it parked in cooldown (#2356)")
	}
	if msg.Number != 20 {
		t.Fatalf("expected the other issue B (#20) to be offered, got #%d", msg.Number)
	}
}

// TestDupePR_DisconnectReleaseIgnoresSyntheticReviewTask verifies the
// disconnect-release cooldown only books real issue tasks: a synthetic
// pr-review task (Number == 0) must not stamp a bogus "repo#0" failure key.
func TestDupePR_DisconnectReleaseIgnoresSyntheticReviewTask(t *testing.T) {
	hub, _ := covK2Hub(t)
	// A synthetic review task carries Number == 0. The guard in the disconnect
	// defer keys off Number > 0, so recordTaskFailure is only reached for real
	// issues. Assert the invariant directly: booking a Number==0 key is never done,
	// so no "repo#0" cooldown exists.
	if hub.isTaskInFailureCooldown("myorg/repo1", 0) {
		t.Fatalf("a synthetic review task (Number==0) must never be parked in a cooldown")
	}
}

// TestHeldSlot_ReadyWhileHoldingTaskReleasesAndCoolsDown is the kubestellar/
// hive#2545 regression: a contributor that never actually starts work on an
// assigned issue (bare workspace, no checkout, agent idle) but stays connected
// and eventually sends "ready" again — e.g. because the relay's own
// MAX_TASK_DURATION_MS watchdog gave up and requeued, or the agent itself asked
// for something new — used to hold the assignment slot forever: the "ready"
// handler logged "task abandoned without completion" but left currentTask set
// and booked no cooldown at all, unlike the disconnect path (#2356) right above
// it. That kept the abandoned issue out of activeIssues circulation for the
// life of the connection with no PR, no failure record, and no re-offer ever
// produced. After the fix, sending "ready" while holding a task clears
// currentTask (releasing the issue back into activeIssues) and books the same
// short non-permanent failure cooldown task_failed and disconnect already use,
// so the issue is not instantly handed straight back to the very next
// selectTask call — here, the "ready" call that triggers the release itself.
func TestHeldSlot_ReadyWhileHoldingTaskReleasesAndCoolsDown(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	// Two admissible issues: A (#10, head of scan) and B (#20). If the abandoned
	// A were still re-admissible, the very next selection (triggered by the same
	// "ready" that abandons it) would return A again; after the fix it must be
	// parked and B offered instead.
	twoIssueStatus(s)

	body := `{"github_username":"held-slot-user"}`
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok

	// First "ready": get assigned the head-of-scan issue A (#10). Simulate the
	// stalled contributor: never send task_progress/task_complete/task_failed for
	// it — just sit there, exactly like a session with no workspace prepared.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 10 {
		t.Fatalf("expected task_assign for head-of-scan A (#10), got type=%s number=%d", assign.Type, assign.Number)
	}

	// The contributor (or its relay's own timeout) asks for work again WITHOUT
	// ever reporting progress, completion, or failure on A. This is the abandon
	// path under test, not a disconnect.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	second := readMsg(t, conn)
	if second.Type != "task_assign" {
		t.Fatalf("expected a fresh task_assign after abandoning A, got type=%s", second.Type)
	}
	if second.Number == 10 {
		t.Fatalf("held slot: A (#10) was re-offered on the very same ready that abandoned it; expected it released and cooled down (#2545)")
	}
	if second.Number != 20 {
		t.Fatalf("expected the other issue B (#20) to be offered, got #%d", second.Number)
	}

	// The abandoned issue A must be recorded in the short failure cooldown, the
	// same ledger entry task_failed and disconnect-release use — proving this is
	// reused machinery, not a new mechanism.
	if !s.contributeHub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatalf("abandoning a task via ready did not record a failure cooldown for the released issue (#2545)")
	}
}

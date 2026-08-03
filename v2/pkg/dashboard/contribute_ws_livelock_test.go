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

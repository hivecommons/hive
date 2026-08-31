package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_ws_reconnect_assignment_test.go is the regression suite for
// kubestellar/hive#5322 — "a reconnect can silently drop the task assignment".
//
// The reported shape: a socket-level reconnect mid-task left the hub reporting
// current_task=null and the issue back in the ready queue as available, while the
// relay was still working it. The relay DOES re-assert over the new socket
// (bin/contributor-relay.sh sends task_accepted + task_progress on auth_ok), and
// the hub's lease-bound resume path DOES adopt it — but only if it is still
// reachable when the assertion lands.
//
// The tests below drive the full socket path (register → auth → assign → drop →
// redial → re-assert) rather than poking the lease registry directly, because the
// failure is an ORDERING failure between two independent goroutines, not a lease
// validation failure. #5310's protocol-Ping work changed WHEN a socket is torn
// down but nothing here depends on it.

// drainForType reads up to a short window looking for a message of the given
// type, returning it (and true) if it arrives.
func drainForType(t *testing.T, conn *websocket.Conn, want string, window time.Duration) (WSMessage, bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WSMessage{}, false
		}
		conn.SetReadDeadline(time.Now().Add(remaining))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return WSMessage{}, false
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Type == want {
			return m, true
		}
	}
}

// waitForLiveConn polls until a live connection for contributorID appears, so a
// test never races the hub's own registration under h.mu.
func waitForLiveConn(t *testing.T, h *ContributeWSHub, contributorID string, window time.Duration) *ContributorConnection {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		for _, c := range h.connections {
			if c.profile != nil && c.profile.ContributorID == contributorID {
				h.mu.RUnlock()
				return c
			}
		}
		h.mu.RUnlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("#5322: no live connection for %s within %s", contributorID, window)
	return nil
}

// heldTaskFor reports the task the hub believes contributorID currently holds,
// across every live connection for that identity (nil when it believes none).
func heldTaskFor(h *ContributeWSHub, contributorID string) *WSTaskAssign {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		c.mu.Lock()
		if c.profile != nil && c.profile.ContributorID == contributorID && c.currentTask != nil {
			task := *c.currentTask
			c.mu.Unlock()
			return &task
		}
		c.mu.Unlock()
	}
	return nil
}

// readyQueueHas reports whether the ready queue is offering repo#number as
// available work.
func readyQueueHas(h *ContributeWSHub, repo string, number int) bool {
	for _, item := range h.ReadyQueue(readyQueueDefaultLimit) {
		if item.Repo == repo && item.Number == number {
			return true
		}
	}
	return false
}

// assignOverSocket registers a contributor, authenticates it, drives a real
// `ready` → task_assign over the wire, and returns the socket, the registration
// reply, and the assignment.
func assignOverSocket(t *testing.T, s *Server, ts *httptest.Server, username string, issueNumber int) (*websocket.Conn, map[string]string, WSMessage) {
	t.Helper()
	conn, reg := registerAndAuth(t, s, ts, username)
	conn.WriteJSON(WSMessage{Type: "ready"})
	assign, ok := drainForType(t, conn, "task_assign", 3*time.Second)
	if !ok {
		t.Fatalf("#5322: setup did not produce a task_assign for %s", username)
	}
	if assign.Number != issueNumber {
		t.Fatalf("#5322: setup assigned %s#%d, wanted #%d", assign.Repo, assign.Number, issueNumber)
	}
	return conn, reg, assign
}

// TestReconnect5322_LateDisconnectDeferDoesNotDropResumedAssignment is the core
// #5322 reproduction.
//
// The hub keys h.connections by a random per-socket connID and does NOT displace
// or even notice a second socket from the same contributor. A socket that dies
// WITHOUT a close frame (the reported bare-`joined`-with-no-`left` signature) has
// a read loop that is still parked in ReadMessage: its disconnect defer — which
// clears currentTask and books a release cooldown on the issue — has not run yet.
// The relay meanwhile redials in ~1s and re-asserts its task, which the new
// connection adopts from the server lease.
//
// The defer then runs LATE, on the OLD connection, for a task the NEW connection
// is now legitimately holding. Because bookReleaseCooldown is keyed by ISSUE, not
// by connection, it stamps a failure cooldown on an issue that is actively in
// flight — and the "released: connection lost" row is written for work nobody
// released. That is the silent drop: the hub's own record of the assignment is
// contradicted by a defer belonging to a socket that no longer represents the
// contributor.
//
// The assertion is on the OBSERVABLE contract from the issue's acceptance
// criteria: after the reconnect settles, the hub reports the task in flight and
// the issue is NOT offered in the ready queue.
func TestReconnect5322_LateDisconnectDeferDoesNotDropResumedAssignment(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_5322")}
	s.contributeHub.server = s
	setStatusIssues(s, intgIssue(5227, "scheduler: thread a per-repo checkout root", "someone", nil))

	conn, reg, assign := assignOverSocket(t, s, ts, "flapper", 5227)
	cid := reg["contributor_id"]

	// Sanity: the hub believes the task is in flight and withholds it from the queue.
	if got := heldTaskFor(s.contributeHub, cid); got == nil {
		t.Fatalf("#5322: hub did not record the assignment at all")
	}
	if readyQueueHas(s.contributeHub, assign.Repo, assign.Number) {
		t.Fatalf("#5322: an assigned issue was still offered in the ready queue before any reconnect")
	}

	old := waitForLiveConn(t, s.contributeHub, cid, 2*time.Second)

	// Cut the socket the way an L7 proxy does: no close frame, no courtesy. The
	// hub's read loop notices only when its next read errors, so its disconnect
	// defer is still pending while the relay redials.
	_ = conn.UnderlyingConn().Close()

	// The relay's reconnect: a brand-new socket, then the exact re-assertion
	// bin/contributor-relay.sh sends on auth_ok — task_accepted followed by a
	// task_progress carrying the server-issued generation.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2) // auth_challenge
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	if m := readMsg(t, conn2); m.Type != "auth_ok" {
		t.Fatalf("redial auth: got %s, want auth_ok", m.Type)
	}
	conn2.WriteJSON(WSMessage{Type: "task_accepted", TaskID: assign.TaskID})
	conn2.WriteJSON(WSMessage{
		Type:    "task_progress",
		TaskID:  assign.TaskID,
		TaskGen: assign.TaskGen,
		Kind:    assign.Kind,
		Repo:    assign.Repo,
		Number:  assign.Number,
		Title:   assign.Title,
		Status:  "working",
	})

	// Let the resume land on the new connection.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.contributeHub.mu.RLock()
		adopted := false
		for _, c := range s.contributeHub.connections {
			if c == old {
				continue
			}
			c.mu.Lock()
			if c.profile != nil && c.profile.ContributorID == cid && c.currentTask != nil {
				adopted = true
			}
			c.mu.Unlock()
		}
		s.contributeHub.mu.RUnlock()
		if adopted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now force the OLD socket's read loop to return, running its disconnect
	// defer late — after the new connection has already re-adopted the task.
	// In production this is simply the kernel reporting the dead peer; here we
	// wait for the deferred cleanup to complete.
	waitForOldConnGone(t, s.contributeHub, old, 5*time.Second)

	// ACCEPTANCE CRITERION 1: the hub reports the task in flight throughout.
	held := heldTaskFor(s.contributeHub, cid)
	if held == nil {
		t.Fatalf("#5322: after a reconnect the hub reports the contributor IDLE — the assignment for %s#%d was silently dropped", assign.Repo, assign.Number)
	}
	if held.TaskID != assign.TaskID || held.Number != assign.Number {
		t.Fatalf("#5322: hub holds %s (%s#%d), want %s (%s#%d)", held.TaskID, held.Repo, held.Number, assign.TaskID, assign.Repo, assign.Number)
	}

	// ACCEPTANCE CRITERION 2: the issue is NOT returned to the ready queue while
	// a connected contributor is working it.
	if readyQueueHas(s.contributeHub, assign.Repo, assign.Number) {
		t.Fatalf("#5322: %s#%d was returned to the ready queue as available while a connected contributor is still working it — a second contributor would redo the work", assign.Repo, assign.Number)
	}

	// ACCEPTANCE CRITERION 3 (the mechanism): the late defer must not have booked
	// a release cooldown against an issue that is legitimately in flight. A
	// cooldown here is what makes the issue un-reassignable later and what marks
	// live work as released in the ledger.
	if s.contributeHub.isTaskInFailureCooldown(assign.Repo, assign.Number) {
		t.Fatalf("#5322: the late disconnect defer booked a release cooldown on %s#%d while the reconnected contributor still holds it", assign.Repo, assign.Number)
	}
}

// TestReconnect5322_DeferBeforeResumeLeavesNoStaleReleaseCooldown pins the OTHER
// ordering of the same flap, deterministically.
//
// Here the dropped socket's disconnect defer completes BEFORE the relay redials —
// the common case, and the one every earlier flap in the report produced (the full
// left / released / joined triplet). The defer books #2356's speculative release
// cooldown, which is correct at the moment it is booked: the hub cannot yet know
// whether the relay is gone or reconnecting, and the window stops a second session
// being handed the same issue during the gap.
//
// The bug is what happens next. The relay comes back and re-adopts the task from
// the server lease, which ANSWERS the question the hedge was standing in for — no
// release ever occurred. Leaving the cooldown stamped means a demonstrably
// in-flight issue carries a "recently released" record for the rest of the window,
// and every surface reading the failure ledger agrees with it.
func TestReconnect5322_DeferBeforeResumeLeavesNoStaleReleaseCooldown(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_5322_early")}
	s.contributeHub.server = s
	setStatusIssues(s, intgIssue(5227, "scheduler: thread a per-repo checkout root", "someone", nil))

	conn, reg, assign := assignOverSocket(t, s, ts, "earlyflap", 5227)
	cid := reg["contributor_id"]
	old := waitForLiveConn(t, s.contributeHub, cid, 2*time.Second)

	// Drop the socket and WAIT for the disconnect defer to complete, so this test
	// exercises the defer-before-resume ordering every time.
	_ = conn.UnderlyingConn().Close()
	waitForOldConnGone(t, s.contributeHub, old, 5*time.Second)

	// The hedge is booked, and is correct at this instant.
	if !s.contributeHub.isTaskInFailureCooldown(assign.Repo, assign.Number) {
		t.Fatalf("#5322: setup expected the disconnect path to book the #2356 release cooldown")
	}

	// The relay returns and re-asserts, exactly as bin/contributor-relay.sh does.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2)
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	if m := readMsg(t, conn2); m.Type != "auth_ok" {
		t.Fatalf("redial auth: got %s, want auth_ok", m.Type)
	}
	conn2.WriteJSON(WSMessage{Type: "task_accepted", TaskID: assign.TaskID})
	conn2.WriteJSON(WSMessage{
		Type: "task_progress", TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Kind: assign.Kind, Repo: assign.Repo, Number: assign.Number,
		Title: assign.Title, Status: "working",
	})
	waitForHeldTask(t, s.contributeHub, cid, assign.TaskID, 3*time.Second)

	// The resume proves the release never happened: withdraw the hedge.
	if s.contributeHub.isTaskInFailureCooldown(assign.Repo, assign.Number) {
		t.Fatalf("#5322: %s#%d still carries a release cooldown after the original relay resumed it from the server lease — an in-flight issue is recorded as recently released", assign.Repo, assign.Number)
	}
	if readyQueueHas(s.contributeHub, assign.Repo, assign.Number) {
		t.Fatalf("#5322: %s#%d offered in the ready queue while the reconnected contributor holds it", assign.Repo, assign.Number)
	}
}

// TestReconnect5322_ClearReleaseCooldownWillNotLaunderARealFailure is the security/
// correctness fence on the withdrawal above. A resume may retract only the
// SPECULATIVE hedge the disconnect path books. A cooldown earned through
// recordTaskFailure — a real task_failed, the relay's watchdog giving up via
// "ready", or the wedged-task lease backstop — carries a consecutive-failure count
// and must survive, or a relay could clear its own failure record by flapping.
func TestReconnect5322_ClearReleaseCooldownWillNotLaunderARealFailure(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	h := s.contributeHub

	// A genuine failure: recordTaskFailure stamps the timestamp AND the count.
	h.recordTaskFailure("myorg/repo1", 900, false)
	if !h.isTaskInFailureCooldown("myorg/repo1", 900) {
		t.Fatalf("setup: recordTaskFailure did not put myorg/repo1#900 in cooldown")
	}
	h.clearReleaseCooldown("myorg/repo1", 900)
	if !h.isTaskInFailureCooldown("myorg/repo1", 900) {
		t.Fatalf("#5322: a resume laundered a REAL failure record for myorg/repo1#900 — flapping would clear earned cooldowns")
	}
	if h.recentFailureCount("myorg/repo1", 900) == 0 {
		t.Fatalf("#5322: the consecutive-failure count for myorg/repo1#900 was dropped")
	}

	// A speculative hedge: bookReleaseCooldown stamps the timestamp only, and IS
	// retractable.
	h.bookReleaseCooldown("myorg/repo1", 901)
	if !h.isTaskInFailureCooldown("myorg/repo1", 901) {
		t.Fatalf("setup: bookReleaseCooldown did not put myorg/repo1#901 in cooldown")
	}
	h.clearReleaseCooldown("myorg/repo1", 901)
	if h.isTaskInFailureCooldown("myorg/repo1", 901) {
		t.Fatalf("#5322: a resume did not withdraw the speculative release cooldown on myorg/repo1#901")
	}

	// Retracting a cooldown that was never booked is a no-op, not a panic.
	h.clearReleaseCooldown("myorg/repo1", 902)
	if h.isTaskInFailureCooldown("myorg/repo1", 902) {
		t.Fatalf("#5322: clearReleaseCooldown invented a cooldown for myorg/repo1#902")
	}
}

// TestReconnect5322_GenuineDepartureStillReleases is the negative control, and the
// guard on kubestellar/hive#5151's territory. The re-adoption check must suppress
// the release ONLY when another live connection is demonstrably holding the exact
// task. A contributor that really does go away — no reconnect, no re-adoption —
// must release exactly as before: the same cooldown, the same activity rows, the
// same accounting. This test fails if the fix over-reaches into a real departure.
func TestReconnect5322_GenuineDepartureStillReleases(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_5322_gone")}
	s.contributeHub.server = s
	setStatusIssues(s, intgIssue(7777, "genuinely abandoned issue", "someone", nil))

	conn, reg, assign := assignOverSocket(t, s, ts, "departer", 7777)
	cid := reg["contributor_id"]
	old := waitForLiveConn(t, s.contributeHub, cid, 2*time.Second)

	_ = conn.UnderlyingConn().Close()
	waitForOldConnGone(t, s.contributeHub, old, 5*time.Second)

	if heldTaskFor(s.contributeHub, cid) != nil {
		t.Fatalf("#5322: a genuinely departed contributor is still recorded as holding %s#%d", assign.Repo, assign.Number)
	}
	if !s.contributeHub.isTaskInFailureCooldown(assign.Repo, assign.Number) {
		t.Fatalf("#5322: a genuine departure no longer books the #2356 release cooldown on %s#%d — the duplicate-assign guard regressed", assign.Repo, assign.Number)
	}
	var sawRelease bool
	for _, e := range s.contributeHub.RecentActivity() {
		if e.Username == "departer" && e.Action == "released: connection lost" {
			sawRelease = true
		}
	}
	if !sawRelease {
		t.Fatalf("#5322: a genuine departure no longer records the #5097 \"released: connection lost\" activity row")
	}
}

// TestReconnect5322_StaleSweepDoesNotStrandAResumedTask covers the second route
// into the same silent drop, via cleanupLoop's stale-pong sweep.
//
// The sweep deletes a stale connection from h.connections under h.mu and closes
// it OUTSIDE the lock, relying on the read loop's defer to release the task. But
// the connection is invisible to activeIssues / activeIssueKeys the moment it is
// deleted, while its currentTask is still set on the now-orphaned struct — and
// the defer that eventually runs then books a release cooldown for the issue.
// If the relay has already reconnected and re-adopted in that window, the sweep's
// victim is a socket whose task belongs to somebody else.
func TestReconnect5322_StaleSweepDoesNotStrandAResumedTask(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_5322_sweep")}
	s.contributeHub.server = s
	setStatusIssues(s, intgIssue(4242, "sweep-path issue", "someone", nil))

	conn, reg, assign := assignOverSocket(t, s, ts, "sweeper", 4242)
	defer conn.Close()
	cid := reg["contributor_id"]
	old := waitForLiveConn(t, s.contributeHub, cid, 2*time.Second)

	// A second, live socket for the SAME contributor that has legitimately
	// re-adopted the task from the server lease — exactly the post-reconnect
	// steady state.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2)
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn2)

	// Move the task to the new connection the way the lease-bound resume does.
	fresh := waitForOtherLiveConn(t, s.contributeHub, cid, old, 2*time.Second)
	old.mu.Lock()
	old.currentTask = nil
	old.lastLeaseRenew = time.Time{}
	// Drive the old connection stale so the sweep picks it up.
	old.lastPong = time.Now().Add(-2 * wsHeartbeatTimeout)
	old.mu.Unlock()
	fresh.mu.Lock()
	fresh.currentTask = &WSTaskAssign{TaskID: assign.TaskID, Kind: assign.Kind, Repo: assign.Repo, Number: assign.Number, Title: assign.Title}
	fresh.currentTaskGen = assign.TaskGen
	fresh.lastLeaseRenew = time.Now()
	fresh.mu.Unlock()

	// The issue must be withheld from the queue on the strength of the LIVE
	// holder alone, regardless of what the stale connection is doing.
	if readyQueueHas(s.contributeHub, assign.Repo, assign.Number) {
		t.Fatalf("#5322: %s#%d offered as available while a live connection holds it", assign.Repo, assign.Number)
	}
	if heldTaskFor(s.contributeHub, cid) == nil {
		t.Fatalf("#5322: hub reports the contributor idle while a live connection holds the task")
	}
}

// waitForOldConnGone polls until the given connection has been deregistered from
// the hub, i.e. its read loop returned and its disconnect defer ran to
// completion. That is the moment #5322's late-defer damage, if any, has landed.
func waitForOldConnGone(t *testing.T, h *ContributeWSHub, old *ContributorConnection, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		present := false
		for _, c := range h.connections {
			if c == old {
				present = true
			}
		}
		h.mu.RUnlock()
		if !present {
			// Give the rest of the defer (cooldown booking, activity rows) a
			// beat to finish after the map delete it performs.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("#5322: the dropped connection was never cleaned up within %s", window)
}

// waitForOtherLiveConn polls until a live connection for contributorID that is
// NOT `except` appears.
func waitForOtherLiveConn(t *testing.T, h *ContributeWSHub, contributorID string, except *ContributorConnection, window time.Duration) *ContributorConnection {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		for _, c := range h.connections {
			if c != except && c.profile != nil && c.profile.ContributorID == contributorID {
				h.mu.RUnlock()
				return c
			}
		}
		h.mu.RUnlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("#5322: no second live connection for %s within %s", contributorID, window)
	return nil
}

// waitForHeldTask polls until the hub records contributorID as holding taskID.
func waitForHeldTask(t *testing.T, h *ContributeWSHub, contributorID, taskID string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if held := heldTaskFor(h, contributorID); held != nil && held.TaskID == taskID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("#5322: the hub never recorded %s as holding %s — the reconnect dropped the assignment", contributorID, taskID)
}

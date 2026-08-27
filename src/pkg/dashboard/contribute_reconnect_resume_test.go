package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_reconnect_resume_test.go covers kubestellar/hive#4260: a relay whose
// socket drops and reconnects inside the backoff window must be able to resume the
// task it never stopped working, and a dropped socket must not be counted as a
// failure OF THE ISSUE.
//
// Two independent defects sat behind the one report:
//
//  1. taskLease.expiresAt was stamped once by recordLease at assignment and never
//     moved, while reclaimExpiredLeases measured liveness from lastLeaseRenew, which
//     every task_progress refreshed. The leaseTTL comment claimed the two clocks were
//     aligned. They were not: a task reporting progress for longer than leaseTTL was
//     correctly never reclaimed, yet its lease had silently expired, so the next
//     socket drop could not be resumed.
//
//  2. The disconnect path booked its #2356 cooldown through recordTaskFailure, which
//     also advances consecutiveFailures. Three drops on one issue crossed
//     consecutiveFailureQuarantineThreshold and quarantined an issue nobody failed.

// --- 1. The lease must not expire under a task that is still working -----------

// TestReconnectResume_LeaseSurvivesBeyondTTLWhileProgressing is the core #4260
// regression. A task that has been reporting progress for well over leaseTTL is
// exactly the task reclaimExpiredLeases refuses to reclaim, because it is alive. Its
// lease must still be re-adoptable, or the first socket drop after leaseTTL costs the
// relay its in-flight work.
func TestReconnectResume_LeaseSurvivesBeyondTTLWhileProgressing(t *testing.T) {
	hub, _ := covK2Hub(t)

	const identity = "c-longrunner"
	assigned := time.Now()
	hub.recordLease(identity, "ct-long", "myorg/repo1", 42, "contributor", 9, assigned)

	// Report progress every five minutes for two full lease windows, the way a relay
	// working a long task does.
	last := assigned
	for elapsed := 5 * time.Minute; elapsed <= 2*leaseTTL; elapsed += 5 * time.Minute {
		last = assigned.Add(elapsed)
		hub.renewLease(identity, "ct-long", last)
	}

	// A socket drop one second after the last progress report.
	drop := last.Add(time.Second)
	if hub.lookupLease(identity, "ct-long", "myorg/repo1", 42, 9, drop) == nil {
		t.Fatalf("#4260: a task that has been reporting progress for %v could not be "+
			"re-adopted after a reconnect — the lease expired under work the hub "+
			"itself considers alive", 2*leaseTTL)
	}

	// The window is measured from the last renewal, not from assignment.
	stillGood := last.Add(leaseTTL - time.Minute)
	if hub.lookupLease(identity, "ct-long", "myorg/repo1", 42, 9, stillGood) == nil {
		t.Fatalf("#4260: the lease window is not measured from the last renewal")
	}
}

// TestReconnectResume_LeaseStillExpiresWhenProgressStops proves the backstop survives
// the fix. Renewal extends the window; it does not make a lease immortal. A relay that
// stops reporting is past its re-adoption window exactly leaseTTL after its LAST
// report, matching when reclaimExpiredLeases would reclaim the task.
func TestReconnectResume_LeaseStillExpiresWhenProgressStops(t *testing.T) {
	hub, _ := covK2Hub(t)

	const identity = "c-stalled"
	assigned := time.Now()
	hub.recordLease(identity, "ct-stall", "myorg/repo1", 7, "contributor", 3, assigned)

	lastProgress := assigned.Add(10 * time.Minute)
	hub.renewLease(identity, "ct-stall", lastProgress)

	// Inside the window from the last report: still re-adoptable.
	if hub.lookupLease(identity, "ct-stall", "myorg/repo1", 7, 3, lastProgress.Add(leaseTTL-time.Minute)) == nil {
		t.Fatalf("a lease inside its renewed window was not re-adoptable")
	}
	// Past it: gone, and the stale entry is dropped from the registry.
	if hub.lookupLease(identity, "ct-stall", "myorg/repo1", 7, 3, lastProgress.Add(leaseTTL+time.Minute)) != nil {
		t.Fatalf("#4260 must not make leases immortal: a lease unrenewed for %v was "+
			"still re-adoptable", leaseTTL)
	}
	hub.leaseMu.Lock()
	_, still := hub.leases[identity]
	hub.leaseMu.Unlock()
	if still {
		t.Fatalf("an expired lease was left in the registry instead of being dropped")
	}
}

// TestReconnectResume_RenewalGrantsNoAuthority is the security half. renewLease moves
// one timestamp and nothing else: it cannot create a lease, cross identities, name a
// task the caller does not hold, or resurrect a revoked one — so C4 (no ownership
// rebuilt from client-supplied fields) is untouched by the fix.
func TestReconnectResume_RenewalGrantsNoAuthority(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()

	// It cannot CREATE a lease for an identity that holds none.
	hub.renewLease("c-nobody", "ct-invented", now)
	if hub.lookupLease("c-nobody", "ct-invented", "myorg/repo1", 1, 5, now) != nil {
		t.Fatalf("C4: renewLease minted a lease for an identity the hub never assigned")
	}

	hub.recordLease("c-owner", "ct-owned", "myorg/repo1", 11, "contributor", 4, now)

	// It cannot renew a lease for a DIFFERENT task than the one recorded — a relay
	// naming someone else's task id must not extend the lease it does hold.
	hub.renewLease("c-owner", "ct-someone-elses", now.Add(leaseTTL))
	if hub.lookupLease("c-owner", "ct-owned", "myorg/repo1", 11, 4, now.Add(leaseTTL+time.Minute)) != nil {
		t.Fatalf("C4: a renewal naming a different task extended the caller's lease")
	}

	// It cannot renew ACROSS identities.
	hub.recordLease("c-a", "ct-a", "myorg/repo1", 21, "contributor", 6, now)
	hub.renewLease("c-b", "ct-a", now.Add(leaseTTL))
	if hub.lookupLease("c-a", "ct-a", "myorg/repo1", 21, 6, now.Add(leaseTTL+time.Minute)) != nil {
		t.Fatalf("C4: one identity's renewal extended another identity's lease")
	}

	// It cannot resurrect a REVOKED lease — every release path stays terminal.
	hub.recordLease("c-rev", "ct-rev", "myorg/repo1", 31, "contributor", 8, now)
	hub.revokeLease("c-rev", "ct-rev")
	hub.renewLease("c-rev", "ct-rev", now)
	if hub.lookupLease("c-rev", "ct-rev", "myorg/repo1", 31, 8, now) != nil {
		t.Fatalf("C4: a renewal resurrected a revoked lease — a released worker could " +
			"re-adopt the task the hub took back")
	}

	// Empty operands are no-ops rather than wildcard matches.
	hub.recordLease("c-guard", "ct-guard", "myorg/repo1", 41, "contributor", 12, now)
	hub.renewLease("", "ct-guard", now.Add(leaseTTL))
	hub.renewLease("c-guard", "", now.Add(leaseTTL))
	if hub.lookupLease("c-guard", "ct-guard", "myorg/repo1", 41, 12, now.Add(leaseTTL+time.Minute)) != nil {
		t.Fatalf("C4: an empty identity or task id was treated as a wildcard renewal")
	}
}

// TestReconnectResume_RenewalPreservesTheMatchTuple proves a renewal rewrites only
// expiresAt. The {task, repo, number, tier, generation} tuple lookupLease matches on —
// and the #2568 generation fence built on it — must come through untouched, so a
// renewed lease is no easier to claim than a fresh one.
func TestReconnectResume_RenewalPreservesTheMatchTuple(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	hub.recordLease("c-fence", "ct-fence", "myorg/repo1", 55, "contributor", 17, now)

	hub.leaseMu.Lock()
	before := *hub.leases["c-fence"]
	hub.leaseMu.Unlock()

	renewedAt := now.Add(20 * time.Minute)
	hub.renewLease("c-fence", "ct-fence", renewedAt)

	hub.leaseMu.Lock()
	after := *hub.leases["c-fence"]
	hub.leaseMu.Unlock()

	if after.identity != before.identity || after.taskID != before.taskID ||
		after.repo != before.repo || after.number != before.number ||
		after.tier != before.tier || after.gen != before.gen {
		t.Fatalf("renewal rewrote the match tuple: before=%+v after=%+v", before, after)
	}
	if !after.expiresAt.Equal(renewedAt.Add(leaseTTL)) {
		t.Fatalf("renewal did not move expiresAt to lastRenewal+leaseTTL: got %v", after.expiresAt)
	}

	// #2568: the fence still holds on a renewed lease.
	after10 := renewedAt.Add(time.Minute)
	if hub.lookupLease("c-fence", "ct-fence", "myorg/repo1", 55, 16, after10) != nil {
		t.Fatalf("#2568: a stale generation matched a RENEWED lease — the fence was lost")
	}
	if hub.lookupLease("c-fence", "ct-fence", "myorg/repo1", 55, 0, after10) != nil {
		t.Fatalf("C4: an unversioned resume (gen 0) matched a renewed lease")
	}
	if hub.lookupLease("c-fence", "ct-fence", "myorg/other", 55, 17, after10) != nil {
		t.Fatalf("C4: a wrong-repo resume matched a renewed lease")
	}
	if hub.lookupLease("c-fence", "ct-fence", "myorg/repo1", 56, 17, after10) != nil {
		t.Fatalf("C4: a wrong-number resume matched a renewed lease")
	}
	if hub.lookupLease("c-fence", "ct-fence", "myorg/repo1", 55, 17, after10) == nil {
		t.Fatalf("the exact resume claim did not match its own renewed lease")
	}
}

// TestReconnectResume_ProgressHandlerRenewsLease drives the real task_progress handler
// and asserts the lease registry moved — proving the renewal is actually wired into
// the message path and not merely available as a method.
func TestReconnectResume_ProgressHandlerRenewsLease(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	seedOneIssue(s, 91, "Long-running work")

	conn, _ := registerAndAuth(t, s, ts, "resume-progress-user")
	defer conn.Close()

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 91 {
		t.Fatalf("expected task_assign for #91, got type=%s number=%d", assign.Type, assign.Number)
	}

	identity := onlyLeaseIdentity(t, s.contributeHub)
	firstExpiry := leaseExpiry(t, s.contributeHub, identity)

	// A progress report from the relay, exactly as contributor-relay.sh sends it.
	time.Sleep(10 * time.Millisecond)
	conn.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 2, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Status: "working",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if leaseExpiry(t, s.contributeHub, identity).After(firstExpiry) {
			return // wired
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("#4260: task_progress did not renew the lease registry — expiresAt is "+
		"still %v, so the lease still ages out from assignment while the task works",
		firstExpiry)
}

// TestReconnectResume_HandlerResumesPastOriginalTTL is the end-to-end proof of the
// reported behaviour. A relay is assigned a task, works past the point where the
// lease used to expire, drops its socket, reconnects, and re-asserts the task exactly
// as contributor-relay.sh does. It must be resumed, not told "no active lease for
// this task" and handed the same issue as a brand-new assignment.
func TestReconnectResume_HandlerResumesPastOriginalTTL(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_4260")}
	s.contributeHub.server = s
	seedOneIssue(s, 4203, "Podman: evaluate Quadlet .kube reuse")

	conn, reg := registerAndAuth(t, s, ts, "resume-reconnect-user")

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 4203 {
		t.Fatalf("expected task_assign for #4203, got type=%s number=%d", assign.Type, assign.Number)
	}
	if assign.TaskGen == 0 {
		t.Fatalf("task_assign carried no task_gen — the relay would have nothing to echo")
	}
	identity := onlyLeaseIdentity(t, s.contributeHub)

	// The task has been working for longer than a lease window. Before #4260 the
	// registry entry aged out here even though the relay never stopped reporting;
	// now the last progress report carries it forward.
	backdateLease(t, s.contributeHub, identity, time.Now().Add(-(leaseTTL + 10*time.Minute)))
	conn.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 2, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Status: "working",
	})

	// The socket drops. The disconnect defer runs: currentTask cleared, generation
	// bumped on the dead connection, cooldown booked. The hub's read loop consumes
	// this connection's frames in order, so the progress above is already processed
	// by the time the close is seen — no sleep needed, and deliberately no assertion
	// on the lease here: the point of the test is what the RECONNECT gets back.
	conn.Close()
	waitForFailureCooldown(t, s.contributeHub, "myorg/repo1", 4203)

	// The relay reconnects one backoff later with the SAME registration identity and
	// re-asserts the task it is still working, carrying the generation it was
	// originally assigned.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2) // auth_challenge
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn2) // auth_ok

	conn2.WriteJSON(WSMessage{Type: "task_accepted", Seq: 3, TaskID: assign.TaskID})
	conn2.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 4, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Kind: "issue", Title: assign.Title,
		Status: "working",
	})

	// Nothing may come back revoking or re-assigning the task. A token_refresh is the
	// expected consequence of a successful resume (resumeTaskToken re-arms the
	// credential), so it is accepted, not treated as noise.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		conn2.SetReadDeadline(time.Now().Add(remaining))
		_, raw, rerr := conn2.ReadMessage()
		if rerr != nil {
			break
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "task_revoke":
			t.Fatalf("#4260: the reconnecting relay was told %q and lost its in-flight "+
				"work — the resume the disconnect path documents did not happen", m.Reason)
		case "task_assign":
			t.Fatalf("#4260: the reconnecting relay was handed a NEW assignment for "+
				"%s#%d — this is the frame that types a fresh prompt into a pane whose "+
				"CLI is still mid-turn", m.Repo, m.Number)
		}
	}

	// And the hub must now actually hold the task on the new connection.
	if !hubHoldsTask(s.contributeHub, assign.TaskID) {
		t.Fatalf("#4260: the resume produced no revoke, but the hub does not hold the " +
			"task on the reconnected session either")
	}
}

// --- 2. A dropped socket is not a failure of the issue -------------------------

// TestDisconnectCooldown_DoesNotQuarantineTheIssue is the second, separable half of
// #4260. The disconnect cooldown exists to stop a SECOND session picking up the issue
// during the reconnect window (#2356). Routing it through recordTaskFailure also
// advanced the consecutive-failure counter, so three dropped sockets on one issue —
// ordinary on a flaky connection across a long session — quarantined a perfectly
// workable issue for quarantineCooldownHours.
func TestDisconnectCooldown_DoesNotQuarantineTheIssue(t *testing.T) {
	hub, _ := covK2Hub(t)

	for i := 0; i < consecutiveFailureQuarantineThreshold; i++ {
		hub.bookReleaseCooldown("myorg/repo1", 4144)
	}

	if got := hub.recentFailureCount("myorg/repo1", 4144); got != 0 {
		t.Fatalf("#4260: %d disconnects advanced the consecutive-failure counter to %d — "+
			"nothing failed", consecutiveFailureQuarantineThreshold, got)
	}

	hub.completedMu.Lock()
	window := hub.failureCooldownForLocked("myorg/repo1#4144")
	hub.completedMu.Unlock()
	if window != failedTaskCooldownMinutes*time.Minute {
		t.Fatalf("#4260: %d disconnects earned the %v quarantine instead of the %v "+
			"reconnect cooldown", consecutiveFailureQuarantineThreshold,
			quarantineCooldownHours*time.Hour, failedTaskCooldownMinutes*time.Minute)
	}
}

// TestDisconnectCooldown_KeepsTheDuplicatePRGuard proves the #2356 guarantee is
// untouched: the cooldown the disconnect books still excludes the issue from
// selection, which is the whole reason it is booked.
func TestDisconnectCooldown_KeepsTheDuplicatePRGuard(t *testing.T) {
	hub, _ := covK2Hub(t)

	if hub.isTaskInFailureCooldown("myorg/repo1", 4203) {
		t.Fatalf("issue unexpectedly in cooldown before the release")
	}
	hub.bookReleaseCooldown("myorg/repo1", 4203)
	if !hub.isTaskInFailureCooldown("myorg/repo1", 4203) {
		t.Fatalf("#2356: a released issue was instantly re-admissible — a second " +
			"session could take it while the original relay reconnects, and both file PRs")
	}
}

// TestDisconnectCooldown_RealFailuresStillQuarantine is the control. Dropping the
// counter for disconnects must not disarm quarantine for things that genuinely fail:
// task_failed, the relay's own watchdog abandoning via "ready", and the wedged-task
// lease backstop all still call recordTaskFailure and still count.
func TestDisconnectCooldown_RealFailuresStillQuarantine(t *testing.T) {
	hub, _ := covK2Hub(t)

	for i := 0; i < consecutiveFailureQuarantineThreshold; i++ {
		hub.recordTaskFailure("myorg/repo1", 99, false)
	}
	if got := hub.recentFailureCount("myorg/repo1", 99); got != consecutiveFailureQuarantineThreshold {
		t.Fatalf("real failures stopped counting: got %d, want %d",
			got, consecutiveFailureQuarantineThreshold)
	}
	hub.completedMu.Lock()
	window := hub.failureCooldownForLocked("myorg/repo1#99")
	hub.completedMu.Unlock()
	if window != quarantineCooldownHours*time.Hour {
		t.Fatalf("#2435 regression: %d real failures no longer quarantine (window %v)",
			consecutiveFailureQuarantineThreshold, window)
	}

	// A disconnect mixed in with real failures neither adds to nor erases the count.
	hub.bookReleaseCooldown("myorg/repo1", 99)
	if got := hub.recentFailureCount("myorg/repo1", 99); got != consecutiveFailureQuarantineThreshold {
		t.Fatalf("a disconnect changed an existing failure count: got %d, want %d",
			got, consecutiveFailureQuarantineThreshold)
	}
}

// TestDisconnectCooldown_HandlerBooksWithoutCounting drives a real socket drop through
// the disconnect defer and asserts both halves at once: the #2356 cooldown is on the
// issue, and the quarantine counter never moved.
func TestDisconnectCooldown_HandlerBooksWithoutCounting(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	seedOneIssue(s, 4260, "Relay reconnect resume")

	conn, _ := registerAndAuth(t, s, ts, "disconnect-count-user")
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 4260 {
		t.Fatalf("expected task_assign for #4260, got type=%s number=%d", assign.Type, assign.Number)
	}

	conn.Close()
	waitForFailureCooldown(t, s.contributeHub, "myorg/repo1", 4260)

	if got := s.contributeHub.recentFailureCount("myorg/repo1", 4260); got != 0 {
		t.Fatalf("#4260: the disconnect handler counted a dropped socket as a failure "+
			"of the issue (count=%d)", got)
	}
}

// --- helpers -------------------------------------------------------------------

// seedOneIssue puts a single admissible issue in the status payload so `ready`
// yields a task_assign for it.
func seedOneIssue(s *Server, number int, title string) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "repo1",
			Full: "myorg/repo1",
			ActionableIssues: []any{map[string]any{
				"number": float64(number),
				"title":  title,
				"url":    "https://github.com/myorg/repo1/issues/" + itoa(number),
				"author": "someone",
			}},
		}},
	}
	s.statusMu.Unlock()
}

// onlyLeaseIdentity returns the identity of the single lease the hub holds, failing
// if there is not exactly one — the tests below assume one assignment in flight.
func onlyLeaseIdentity(t *testing.T, h *ContributeWSHub) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.leaseMu.Lock()
		n := len(h.leases)
		var id string
		for k := range h.leases {
			id = k
		}
		h.leaseMu.Unlock()
		if n == 1 {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly one server-issued lease, found %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func leaseExpiry(t *testing.T, h *ContributeWSHub, identity string) time.Time {
	t.Helper()
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l, ok := h.leases[identity]
	if !ok {
		t.Fatalf("no lease recorded for identity %q", identity)
	}
	return l.expiresAt
}

// backdateLease rewinds a lease's expiry to simulate a task that has been working
// long enough for the pre-#4260 assignment-anchored window to have lapsed.
func backdateLease(t *testing.T, h *ContributeWSHub, identity string, lastRenewal time.Time) {
	t.Helper()
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l, ok := h.leases[identity]
	if !ok {
		t.Fatalf("no lease recorded for identity %q", identity)
	}
	l.expiresAt = lastRenewal.Add(leaseTTL)
}

func waitForFailureCooldown(t *testing.T, h *ContributeWSHub, repo string, number int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.isTaskInFailureCooldown(repo, number) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("disconnect did not book the #2356 cooldown for %s#%d", repo, number)
}

// hubHoldsTask reports whether any live connection currently holds the given task.
func hubHoldsTask(h *ContributeWSHub, taskID string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		for _, c := range h.connections {
			c.mu.Lock()
			held := c.currentTask != nil && c.currentTask.TaskID == taskID
			c.mu.Unlock()
			if held {
				h.mu.RUnlock()
				return true
			}
		}
		h.mu.RUnlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

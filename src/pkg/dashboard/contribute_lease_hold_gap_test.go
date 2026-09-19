package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// contribute_lease_hold_gap_test.go — hivecommons/hive#7773.
//
// Two clocks were supposed to keep one issue with one contributor after a socket
// dropped: #2356's release cooldown (failedTaskCooldownMinutes, ten minutes) kept
// the issue out of selectTask, and the lease (leaseTTL, thirty minutes from the
// last progress report) let the returning relay resume it. Nothing covered the
// twenty minutes between them: the issue was out of the live-connection scan,
// out of cooldown, and still resumable, so a second contributor's `ready` was
// answered with it and the first relay then resumed it too. This drives exactly
// that ordering over the real protocol and asserts the issue stays held for as
// long as it can be resumed — and that the first relay's resume still works.

// ageDisconnectHedge moves the release cooldown stamp for repo#number back by d,
// as if that much wall-clock had passed since the socket dropped.
func ageDisconnectHedge(t *testing.T, h *ContributeWSHub, key string, d time.Duration) {
	t.Helper()
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	stamp, ok := h.failedTasks[key]
	if !ok {
		t.Fatalf("no release cooldown booked for %q", key)
	}
	h.failedTasks[key] = stamp.Add(-d)
}

func TestLeaseHold_OutageBetweenCooldownAndLeaseDoesNotDoubleAssign(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_hold_7773")}
	s.contributeHub.server = s
	seedOneIssue(s, 7773, "Issue worked through a medium-length outage")
	const key = "myorg/repo1#7773"

	// t=0: contributor A is assigned X and reports progress.
	connA, regA := registerAndAuth(t, s, ts, "outage-relay")
	connA.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, connA)
	if assign.Type != "task_assign" || assign.Number != 7773 {
		t.Fatalf("expected task_assign for #7773, got %+v", assign)
	}
	connA.WriteJSON(WSMessage{Type: "task_progress", Seq: 2, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Status: "working"})

	// A's machine drops off the network. The hedge is booked; the lease is kept.
	connA.Close()
	waitForFailureCooldown(t, s.contributeHub, "myorg/repo1", 7773)

	// Fifteen minutes pass: past the ten-minute release cooldown, inside the
	// thirty-minute lease. selectTask reads the wall clock, so the stamps are
	// aged rather than the test slept.
	const outage = 15 * time.Minute
	ageDisconnectHedge(t, s.contributeHub, key, outage)
	if s.contributeHub.isTaskInFailureCooldownKey(key) {
		t.Fatalf("setup: the release cooldown must have lapsed after %v", outage)
	}
	identity := onlyLeaseIdentity(t, s.contributeHub)
	if s.contributeHub.lookupLease(identity, assign.TaskID, "myorg/repo1", 7773, assign.TaskGen, time.Now()) == nil {
		t.Fatalf("setup: the lease must still be re-adoptable after %v (leaseTTL %v)", outage, leaseTTL)
	}

	// Contributor B asks for work. Before #7773 this was answered with X.
	connB, _ := registerAndAuth(t, s, ts, "second-contributor")
	defer connB.Close()
	connB.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	answer := readMsg(t, connB)
	if answer.Type == "task_assign" {
		t.Fatalf("#7773: issue #%d was offered to a second contributor while its first holder's "+
			"lease was still re-adoptable — the double assignment #2356 exists to prevent: %+v", answer.Number, answer)
	}
	if answer.Type != "task_unavailable" {
		t.Fatalf("expected task_unavailable for the second contributor, got %s", answer.Type)
	}

	// A's relay comes back and re-asserts X. The hold must not have cost it the
	// resume that is the whole point of keeping the lease.
	connA2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer connA2.Close()
	readMsg(t, connA2)
	connA2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: regA["registration_token"], CLIBackend: "claude"})
	readMsg(t, connA2)
	connA2.WriteJSON(WSMessage{Type: "task_accepted", Seq: 3, TaskID: assign.TaskID})
	connA2.WriteJSON(WSMessage{Type: "task_progress", Seq: 4, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Kind: "issue", Title: assign.Title, Status: "working"})
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		connA2.SetReadDeadline(time.Now().Add(remaining))
		_, raw, rerr := connA2.ReadMessage()
		if rerr != nil {
			break
		}
		var m WSMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case "task_revoke":
			t.Fatalf("the returning relay was revoked (%q); the hold must not cost the resume", m.Reason)
		case "task_assign":
			t.Fatalf("the returning relay was handed a fresh assignment instead of resuming")
		}
	}
	if !hubHoldsTask(s.contributeHub, assign.TaskID) {
		t.Fatalf("the returning relay did not resume its task")
	}

	// And once the holder has released the task, the hold is gone at once: B is
	// offered it on its next ask.
	connA2.WriteJSON(WSMessage{Type: "task_complete", Seq: 5, TaskID: assign.TaskID, TaskGen: assign.TaskGen, Result: "completed"})
	deadline = time.Now().Add(2 * time.Second)
	for {
		s.contributeHub.leaseMu.Lock()
		_, held := s.contributeHub.leases[identity]
		s.contributeHub.leaseMu.Unlock()
		if !held || time.Now().After(deadline) {
			if held {
				t.Fatalf("task_complete did not revoke the lease")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if keys := s.contributeHub.leasedIssueKeys("anyone-else", time.Now()); keys[key] {
		t.Fatalf("a released task must not stay held: %v", keys)
	}
}

// TestLeaseHold_ExpiredLeaseReleasesTheIssue pins the other edge: once the lease
// can no longer be resumed, nothing holds the issue and it is re-offered — the
// hold lasts exactly as long as the resumability it protects.
func TestLeaseHold_ExpiredLeaseReleasesTheIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	seedTwoIssues(s, 10, 20)
	hub.recordLease("c-gone", "ct-gone", "myorg/repo1", 10, "contributor", 5, time.Now())

	other := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "newcomer", ContributorID: "c-newcomer", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg := hub.selectTask(other)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 20 {
		t.Fatalf("with #10 leased to a disconnected holder, the newcomer must get #20; got %+v", msg)
	}

	// The holder never comes back; the lease ages out.
	hub.leaseMu.Lock()
	hub.leases["c-gone"].expiresAt = time.Now().Add(-time.Second)
	hub.leaseMu.Unlock()
	seedTwoIssues(s, 10, 20)
	another := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "later", ContributorID: "c-later", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	// #20 is now leased to the newcomer (not a live connection here, so only the
	// lease holds it), and #10's lease has expired: exactly #10 is offerable.
	msg = hub.selectTask(another)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 10 {
		t.Fatalf("expected #10 to be re-offered once its lease expired (and #20 to stay held by its live lease), got %+v", msg)
	}
	if hub.leasedIssueKeys("c-later", time.Now())["myorg/repo1#10"] {
		t.Fatalf("an expired lease must not hold #10")
	}
}

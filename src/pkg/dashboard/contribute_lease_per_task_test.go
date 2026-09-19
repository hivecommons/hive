package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/internal/testutil"
)

// contribute_lease_per_task_test.go — hivecommons/hive#7774.
//
// The concurrency gate lets one contributor identity hold max_concurrent tasks
// across its live connections, but the lease registry was keyed by identity, so
// the second assignment silently evicted the first task's lease. A flap on the
// connection working the first task then found only the second's lease: the
// resume was rejected, the relay was sent task_revoke, its agent was
// interrupted mid-turn, and the issue was requeued as failed. The registry now
// holds one lease per task, so every task the hub issued and has not released
// is independently re-adoptable.

// TestLeasePerTask_SecondAssignmentKeepsFirstLease is the registry-level pin:
// two tasks recorded for one identity are two leases, each looked up on its own,
// each renewed and revoked on its own.
func TestLeasePerTask_SecondAssignmentKeepsFirstLease(t *testing.T) {
	hub, _ := covK2Hub(t)
	now := time.Now()
	const identity = "c-two"
	hub.recordLease(identity, "ct-x", "myorg/repo1", 1, "contributor", 11, now)
	hub.recordLease(identity, "ct-y", "myorg/repo1", 2, "contributor", 12, now)

	if hub.lookupLease(identity, "ct-x", "myorg/repo1", 1, 11, now) == nil {
		t.Fatalf("the first task's lease was evicted by the second assignment to the same identity")
	}
	if hub.lookupLease(identity, "ct-y", "myorg/repo1", 2, 12, now) == nil {
		t.Fatalf("the second task's lease is missing")
	}

	// Renewing one moves only that one.
	later := now.Add(10 * time.Minute)
	hub.renewLease(identity, "ct-x", later)
	hub.leaseMu.Lock()
	xExp := hub.leaseForLocked(identity, "ct-x").expiresAt
	yExp := hub.leaseForLocked(identity, "ct-y").expiresAt
	hub.leaseMu.Unlock()
	if !xExp.Equal(later.Add(leaseTTL)) || !yExp.Equal(now.Add(leaseTTL)) {
		t.Fatalf("renewal leaked across tasks: x=%v y=%v", xExp, yExp)
	}

	// Revoking one leaves the other re-adoptable.
	hub.revokeLease(identity, "ct-y")
	if hub.lookupLease(identity, "ct-y", "myorg/repo1", 2, 12, now) != nil {
		t.Fatalf("revoked lease still present")
	}
	if hub.lookupLease(identity, "ct-x", "myorg/repo1", 1, 11, now) == nil {
		t.Fatalf("revoking the second task's lease took the first's with it")
	}

	// The persisted registry carries both, and a restart brings both back.
	hub.recordLease(identity, "ct-y", "myorg/repo1", 2, "contributor", 13, now)
	hub2 := restartedHub(t)
	if hub2.lookupLease(identity, "ct-x", "myorg/repo1", 1, 11, now) == nil ||
		hub2.lookupLease(identity, "ct-y", "myorg/repo1", 2, 13, now) == nil {
		t.Fatalf("a restart restored fewer than the two leases the identity held")
	}
}

// TestLeasePerTask_FlapOnFirstTaskResumes is the reproduction from the issue,
// driven through the real handlers: one identity, two connections, two tasks;
// the connection working the FIRST task flaps and re-asserts it. Before #7774
// the hub answered task_revoke ("no active lease for this task") because only
// the second task's lease existed.
func TestLeasePerTask_FlapOnFirstTaskResumes(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	// Replacing deps drops the test Config, so no tier limit applies and the
	// identity may hold two tasks — the max_concurrent > 1 shape the bug needs.
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_7774")}
	s.contributeHub.server = s
	setStatusIssues(s,
		intgIssue(7701, "first task, worked on connection A", "someone", nil),
		intgIssue(7702, "second task, worked on connection B", "someone", nil),
	)

	connA, reg := registerAndAuth(t, s, ts, "two-relays-one-identity")
	connA.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assignX := readMsg(t, connA)
	if assignX.Type != "task_assign" {
		t.Fatalf("connection A expected task_assign, got %+v", assignX)
	}

	// A second relay under the SAME identity: same token, no session label.
	connB, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.Close()
	readMsg(t, connB) // auth_challenge
	connB.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, connB) // auth_ok
	connB.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assignY := readMsg(t, connB)
	if assignY.Type != "task_assign" || assignY.TaskID == assignX.TaskID {
		t.Fatalf("connection B expected a DIFFERENT task_assign, got %+v", assignY)
	}
	identity := onlyIdentityOf(t, s.contributeHub)
	s.contributeHub.leaseMu.Lock()
	xLease := s.contributeHub.leaseForLocked(identity, assignX.TaskID)
	yLease := s.contributeHub.leaseForLocked(identity, assignY.TaskID)
	s.contributeHub.leaseMu.Unlock()
	if xLease == nil || yLease == nil {
		t.Fatalf("both tasks must hold a lease after the second assignment: x=%v y=%v", xLease != nil, yLease != nil)
	}

	// Connection A flaps while still working X.
	connA.Close()
	waitForFailureCooldown(t, s.contributeHub, "myorg/repo1", assignX.Number)

	connA2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer connA2.Close()
	readMsg(t, connA2)
	connA2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, connA2)
	connA2.WriteJSON(WSMessage{Type: "task_accepted", Seq: 2, TaskID: assignX.TaskID})
	connA2.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 3, TaskID: assignX.TaskID, TaskGen: assignX.TaskGen,
		Repo: assignX.Repo, Number: assignX.Number, Kind: "issue", Title: assignX.Title, Status: "working",
	})

	// The resume must be honored: no revoke, no fresh assignment.
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
			t.Fatalf("#7774: the reconnecting relay was told %q for the first of the identity's two tasks — "+
				"its agent is interrupted mid-turn and the issue requeued as failed", m.Reason)
		case "task_assign":
			t.Fatalf("#7774: the reconnecting relay was handed a NEW assignment for %s#%d instead of resuming its own", m.Repo, m.Number)
		}
	}
	if !hubHoldsTask(s.contributeHub, assignX.TaskID) {
		t.Fatalf("the hub does not hold the first task on the reconnected session")
	}
	// And the second task's lease was never touched by any of it.
	s.contributeHub.leaseMu.Lock()
	yAfter := s.contributeHub.leaseForLocked(identity, assignY.TaskID)
	s.contributeHub.leaseMu.Unlock()
	if yAfter == nil || yAfter.gen != assignY.TaskGen {
		t.Fatalf("the second task's lease was disturbed by the first task's resume: %+v", yAfter)
	}
	if !hubHoldsTask(s.contributeHub, assignY.TaskID) {
		t.Fatalf("the second task is no longer held by its connection")
	}
}

// onlyIdentityOf returns the one contributor identity the hub's leases belong
// to, failing if the leases name more than one.
func onlyIdentityOf(t *testing.T, h *ContributeWSHub) string {
	t.Helper()
	var id string
	testutil.Eventually(t, 2*time.Second, func() bool {
		h.leaseMu.Lock()
		defer h.leaseMu.Unlock()
		ids := map[string]bool{}
		for _, l := range h.leases {
			ids[l.identity] = true
		}
		if len(ids) > 1 {
			t.Fatalf("leases belong to %d identities, expected one", len(ids))
		}
		if len(ids) == 1 {
			for k := range ids {
				id = k
			}
			return true
		}
		return false
	}, "no lease recorded")
	return id
}

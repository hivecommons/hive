package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// N9 (CWE-862/639): a task_complete / task_failed carrying the WRONG task_id
// must not clear the assignment.
//
// Both handlers computed `hasTask` from a TaskID equality check and then cleared
// currentTask, pendingToken, credentialDelivered and tokenMintedAt
// UNCONDITIONALLY — before the `if hasTask` block — while every protective
// action (revokeLease, markTaskCompleted / recordTaskFailure, verifyReportedPR)
// sat INSIDE it. The else branch logged "ignored", which was false: the state
// had already been mutated.
//
// The result: a contributor capped at max_concurrent=1 could send
// {"type":"task_complete","task_id":"anything-wrong"} to clear its assignment
// without the lease being revoked or a cooldown booked, go `ready` again, and
// be minted a SECOND live repo credential. Repeat for N.
//
// The Gate (#2568) does not stop this. generationAccepted returns true whenever
// clientGen == 0, and a client that simply OMITS task_gen gets 0 — so the guard
// is opt-in from the client side, which is exactly what an attacker chooses.
//
// The existing coverage misses it in both directions: contribute_ws_test.go's
// wrong-id case asserts only profile counters (which the bug does not touch),
// and contribute_lease_test.go's Gate test always sends a NON-ZERO TaskGen, so
// it never reaches the TaskGen=0 + wrong-id combination.

// leaseCountFor reports how many leases the hub holds for an identity. The lease
// map is the server-side accounting the credential cap is enforced from, so it
// is the thing that must survive a bogus completion.
func leaseCountFor(h *ContributeWSHub, identity string) int {
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	n := 0
	for id := range h.leases {
		if id == identity {
			n++
		}
	}
	return n
}

// TestWrongTaskIDCompletionDoesNotClearAssignment is the N9 regression: a
// completion for a task the contributor does not hold must be a no-op.
func TestWrongTaskIDCompletionDoesNotClearAssignment(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"n9user"}`
	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(body))
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

	setStatusIssues(s, intgIssue(900, "N9 issue", "someone", nil))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}
	realTaskID := assign.TaskID

	// Send a completion for a task this contributor was never assigned, with
	// task_gen OMITTED — the shape the official relay uses, and the shape that
	// slips past the Gate.
	conn.WriteJSON(WSMessage{
		Type:   "task_complete",
		TaskID: "not-the-assigned-task",
		Result: "pr_created",
		PRURL:  "https://github.com/myorg/repo1/pull/1",
	})
	time.Sleep(80 * time.Millisecond)

	// The assignment must survive. Before the fix currentTask was nil here, which
	// is what let the contributor go `ready` and collect a second credential.
	s.contributeHub.mu.RLock()
	var held *ContributorConnection
	for _, c := range s.contributeHub.connections {
		if c.profile != nil && c.profile.GitHubUsername == "n9user" {
			held = c
		}
	}
	s.contributeHub.mu.RUnlock()
	if held == nil {
		t.Fatal("connection vanished")
	}

	held.mu.Lock()
	cur := held.currentTask
	held.mu.Unlock()

	if cur == nil {
		t.Fatal("N9: a wrong-task_id completion CLEARED the assignment — the contributor " +
			"can now go ready and be minted a second concurrent credential under max_concurrent=1")
	}
	if cur.TaskID != realTaskID {
		t.Fatalf("N9: assignment changed to %q, want the original %q", cur.TaskID, realTaskID)
	}

	// The lease is the server-side accounting the cap is enforced from; a bogus
	// completion must not release it either.
	if n := leaseCountFor(s.contributeHub, identityOf(held)); n != 1 {
		t.Fatalf("N9: lease count is %d after a wrong-id completion, want 1 (the lease must not be released)", n)
	}
}

// TestWrongTaskIDFailureDoesNotClearAssignment covers task_failed, which carries
// the identical hole — and additionally books no failure cooldown, so the
// abandoned issue becomes instantly re-assignable.
func TestWrongTaskIDFailureDoesNotClearAssignment(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"n9fail"}`
	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(body))
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
	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn)

	setStatusIssues(s, intgIssue(901, "N9 fail issue", "someone", nil))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}

	conn.WriteJSON(WSMessage{Type: "task_failed", TaskID: "not-the-assigned-task", Permanent: false})
	time.Sleep(80 * time.Millisecond)

	s.contributeHub.mu.RLock()
	var held *ContributorConnection
	for _, c := range s.contributeHub.connections {
		if c.profile != nil && c.profile.GitHubUsername == "n9fail" {
			held = c
		}
	}
	s.contributeHub.mu.RUnlock()
	if held == nil {
		t.Fatal("connection vanished")
	}
	held.mu.Lock()
	cur := held.currentTask
	held.mu.Unlock()

	if cur == nil {
		t.Fatal("N9: a wrong-task_id task_failed CLEARED the assignment without booking a " +
			"failure cooldown — the issue is instantly re-admissible and the credential stays live")
	}
}

// TestCorrectTaskIDCompletionStillClears is the control: the fix must not break
// the legitimate path. A matching task_id still clears the assignment and
// releases the lease.
func TestCorrectTaskIDCompletionStillClears(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()

	body := `{"github_username":"n9ok"}`
	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(body))
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
	readMsg(t, conn)
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn)

	setStatusIssues(s, intgIssue(902, "N9 control issue", "someone", nil))
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", assign)
	}

	conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: assign.TaskID, Result: "pr_created"})
	time.Sleep(80 * time.Millisecond)

	s.contributeHub.mu.RLock()
	var held *ContributorConnection
	for _, c := range s.contributeHub.connections {
		if c.profile != nil && c.profile.GitHubUsername == "n9ok" {
			held = c
		}
	}
	s.contributeHub.mu.RUnlock()
	if held == nil {
		t.Fatal("connection vanished")
	}
	held.mu.Lock()
	cur := held.currentTask
	held.mu.Unlock()

	if cur != nil {
		t.Fatalf("a MATCHING task_complete must still clear the assignment, but currentTask is %q", cur.TaskID)
	}
}

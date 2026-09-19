package dashboard

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/internal/testutil"
)

// contribute_resume_external_key_test.go — hivecommons/hive#7770.
//
// Every guard that stops two contributors working one item keys on the item's
// canonical identity (worksource.Ref.Key, #4245): "owner/repo#42" for a GitHub
// issue, "owner/repo!ENG-123" for a Linear/Jira item. The reconnect resume in
// handleTaskProgress rebuilt currentTask from the lease's repo and number
// alone — the pre-#4245 shape — so a resumed external item (Number 0) had an
// identity of "": it dropped out of the activeIssues double-assignment guard,
// and completing or failing it booked a cooldown against "". The lease had
// carried the key all along.
//
// The disconnect hedge (#2356, bookReleaseCooldown) had the same gap from the
// other side: it was booked only for Number > 0, so an external item had no
// cover during the reconnect window either. Both halves are exercised here
// against a real hub, through the same frames a relay sends.

// seedOneExternalItem puts a single Linear item, and nothing else, in the queue.
func seedOneExternalItem(s *Server, externalID, title string) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name:             "repo1",
			Full:             "myorg/repo1",
			ActionableIssues: []any{externalItem("linear", externalID, title)},
		}},
	}
	s.statusMu.Unlock()
}

// heldIdentityKey polls until some live connection holds taskID and returns
// that connection's currentTask.identityKey(); "" if nothing holds it in time.
func heldIdentityKey(t *testing.T, h *ContributeWSHub, taskID string) string {
	t.Helper()
	var key string
	testutil.Eventually(t, 2*time.Second, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		for _, c := range h.connections {
			c.mu.Lock()
			held := c.currentTask != nil && c.currentTask.TaskID == taskID
			if held {
				key = c.currentTask.identityKey()
			}
			c.mu.Unlock()
			if held {
				return true
			}
		}
		return false
	}, "no live connection held task %q within the wait", taskID)
	return key
}

func waitForFailureCooldownKey(t *testing.T, h *ContributeWSHub, key string) {
	t.Helper()
	testutil.Eventually(t, 2*time.Second, func() bool {
		return h.isTaskInFailureCooldownKey(key)
	}, "disconnect did not book the #2356 hedge for %q — an external item has no cover during the reconnect window", key)
}

func waitForNoFailureCooldownKey(t *testing.T, h *ContributeWSHub, key string) {
	t.Helper()
	testutil.Eventually(t, 2*time.Second, func() bool {
		return !h.isTaskInFailureCooldownKey(key)
	}, "the resume did not withdraw the #2356 hedge for %q (#5322)", key)
}

func TestReconnectResume_ExternalItemKeepsCanonicalKey(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_7770")}
	s.contributeHub.server = s
	seedOneExternalItem(s, "ENG-1", "Linear item worked across a reconnect")
	const wantKey = "myorg/repo1!ENG-1"

	conn, reg := registerAndAuth(t, s, ts, "resume-external-user")

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.TaskKey != wantKey || assign.Number != 0 {
		t.Fatalf("expected task_assign for %s with number 0, got type=%s key=%q number=%d", wantKey, assign.Type, assign.TaskKey, assign.Number)
	}
	if got := heldIdentityKey(t, s.contributeHub, assign.TaskID); got != wantKey {
		t.Fatalf("setup: the fresh assignment must already carry the key, got %q", got)
	}

	// The socket drops. The disconnect hedge must cover the external item by
	// its real key — before #7770 it was booked only for Number > 0, so an
	// external item had no cover at all during the reconnect window.
	conn.Close()
	waitForFailureCooldownKey(t, s.contributeHub, wantKey)

	// The relay reconnects and re-asserts the task against its lease, as the
	// reconnect section of contributor-relay.md describes. A relay written
	// before #4245 carries only repo/number; the key must come from the lease.
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2) // auth_challenge
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn2) // auth_ok
	conn2.WriteJSON(WSMessage{Type: "task_accepted", Seq: 2, TaskID: assign.TaskID})
	conn2.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 3, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: 0, Kind: "issue", Title: assign.Title, Status: "working",
	})

	// (a) The resumed connection reports the item's real canonical key.
	if got := heldIdentityKey(t, s.contributeHub, assign.TaskID); got != wantKey {
		t.Fatalf("after the resume the live connection's currentTask.identityKey() = %q, want %q — "+
			"an external item rebuilt from repo/number alone has no identity, so every key-based guard skips it", got, wantKey)
	}
	// And the hedge it no longer needs is withdrawn, by that same key.
	waitForNoFailureCooldownKey(t, s.contributeHub, wantKey)

	// (b) A second contributor asking for work is NOT offered the item the
	// first is still working: with the key restored it is in activeIssues.
	connB, _ := registerAndAuth(t, s, ts, "second-contributor")
	defer connB.Close()
	connB.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	answer := readMsg(t, connB)
	if answer.Type == "task_assign" {
		t.Fatalf("the item the first contributor is still working was offered to a second one: %+v", answer)
	}
	if answer.Type != "task_unavailable" {
		t.Fatalf("expected task_unavailable for the second contributor, got %s", answer.Type)
	}

	// (c) Completing the resumed item books its completion cooldown against
	// the real key, not against "" — which markTaskCompletedVerdictKeySignal
	// silently drops.
	conn2.WriteJSON(WSMessage{Type: "task_complete", Seq: 4, TaskID: assign.TaskID, TaskGen: assign.TaskGen, Result: "completed"})
	testutil.Eventually(t, 2*time.Second, func() bool {
		s.contributeHub.completedMu.Lock()
		defer s.contributeHub.completedMu.Unlock()
		_, cooled := s.contributeHub.completedTasks[wantKey]
		return cooled
	}, "completing the resumed external item booked no completion cooldown for %q", wantKey)
}

// TestReconnectResume_GitHubItemUnchanged pins the no-regression leg: a GitHub
// issue's identity was never lost on resume (Number > 0 recovers it), and the
// hedge it gets on disconnect is keyed exactly as before.
func TestReconnectResume_GitHubItemUnchanged(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{GHAppAuth: newSucceedingAppAuth(t, "ghs_resume_7770_gh")}
	s.contributeHub.server = s
	seedOneIssue(s, 4207, "GitHub issue worked across a reconnect")

	conn, reg := registerAndAuth(t, s, ts, "resume-github-user")
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 1})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Number != 4207 {
		t.Fatalf("expected task_assign for #4207, got type=%s number=%d", assign.Type, assign.Number)
	}
	conn.Close()
	// The repo/number spelling and the key spelling are the same string.
	waitForFailureCooldown(t, s.contributeHub, "myorg/repo1", 4207)
	waitForFailureCooldownKey(t, s.contributeHub, "myorg/repo1#4207")

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	defer conn2.Close()
	readMsg(t, conn2)
	conn2.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn2)
	conn2.WriteJSON(WSMessage{
		Type: "task_progress", Seq: 2, TaskID: assign.TaskID, TaskGen: assign.TaskGen,
		Repo: assign.Repo, Number: assign.Number, Kind: "issue", Title: assign.Title, Status: "working",
	})
	if got := heldIdentityKey(t, s.contributeHub, assign.TaskID); got != "myorg/repo1#4207" {
		t.Fatalf("resumed GitHub item identity = %q, want myorg/repo1#4207", got)
	}
	waitForNoFailureCooldownKey(t, s.contributeHub, "myorg/repo1#4207")
}

package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// task_mcp_lease_lifetime_test.go pins how long a remote contributor's task
// MCP lease token works. The hub mints it once, at assignment, and never
// re-sends it; the task lease it is bound to is renewed by every accepted
// task_progress. The token used to carry the FIRST lease window as its own
// expiry, so a healthy task lost its task MCP (HTTP 403) thirty minutes after
// assignment. Its own expiry is now only a hard cap; before the cap the
// server-side lease record decides, and these tests cover both directions.

const leaseLifetimeSecret = "lease-lifetime-secret"

// leaseLifetimeHardCap is the documented upper bound on a lease token's own
// expiry, measured from assignment. It is stated here rather than read from
// the hub's constant so that moving the bound is a deliberate edit.
const leaseLifetimeHardCap = 24 * time.Hour

func leaseLifetimeConfig() *config.Config {
	return &config.Config{
		TaskMCP:   config.TaskMCPConfig{RemoteEnabled: true},
		Dashboard: config.DashboardConfig{PublicURL: "https://hive.example"},
	}
}

func newLeaseLifetimeServer(t *testing.T) *Server {
	t.Helper()
	// #8287: a grant whose lease cannot be persisted is refused, so the
	// registry lives in a temp dir rather than under /data.
	dir := t.TempDir()
	s := newTestServer()
	s.authToken = leaseLifetimeSecret
	s.deps = &Dependencies{Config: leaseLifetimeConfig()}
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.contributeHub.taskLeasesFile = filepath.Join(dir, "task-leases.json")
	t.Cleanup(s.contributeHub.Close)
	return s
}

// leaseLifetimeCall asks the task MCP endpoint for task_context as bearer.
func leaseLifetimeCall(s *Server, bearer string) *httptest.ResponseRecorder {
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"task_context","arguments":{}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, taskmcp.EndpointPath, strings.NewReader(call))
	req.Header.Set("Authorization", "Bearer "+bearer)
	s.handleContributeMCP(rec, req)
	return rec
}

// leaseLifetimeAssignedAt is when the hub recorded taskID as assigned.
func leaseLifetimeAssignedAt(t *testing.T, h *ContributeWSHub, taskID string) time.Time {
	t.Helper()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		c.mu.Lock()
		held, at := c.currentTask != nil && c.currentTask.TaskID == taskID, c.taskAssignedAt
		c.mu.Unlock()
		if held {
			return at
		}
	}
	t.Fatalf("no connection holds task %s", taskID)
	return time.Time{}
}

// leaseLifetimeAssignOverRelay drives a real ready -> task_assign round trip
// on a hub with task_mcp.remote_enabled, and returns the assignment and when
// the hub recorded it.
func leaseLifetimeAssignOverRelay(t *testing.T) (WSMessage, time.Time) {
	t.Helper()
	s, ts := setupWSTest(t)
	t.Cleanup(ts.Close)
	s.authToken = leaseLifetimeSecret
	s.deps = &Dependencies{Config: leaseLifetimeConfig()}
	s.contributeHub.server = s
	seedOneIssue(s, 4242, "task that runs longer than one lease window")

	conn, _ := registerAndAuth(t, s, ts, "long-task-relay")
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(WSMessage{Type: "ready", Seq: 1}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.MCP == nil || assign.MCP.Token == "" {
		t.Fatalf("expected task_assign with an mcp block, got type=%s reason=%q mcp=%v", assign.Type, assign.Reason, assign.MCP != nil)
	}
	return assign, leaseLifetimeAssignedAt(t, s.contributeHub, assign.TaskID)
}

// leaseTokenLifetimeAtAssignment is how long after assignment the hub lets a
// lease token be presented. It is measured on a real assignment rather than
// restated here, so a test that ages a task follows whatever selectTask
// stamps into the token.
func leaseTokenLifetimeAtAssignment(t *testing.T) time.Duration {
	t.Helper()
	assign, assignedAt := leaseLifetimeAssignOverRelay(t)
	claims, err := taskmcp.VerifyLeaseToken([]byte(leaseLifetimeSecret), assign.MCP.Token, assignedAt)
	if err != nil {
		t.Fatalf("the token in task_assign does not verify at assignment: %v", err)
	}
	return claims.ExpiresAt.Sub(assignedAt)
}

// assertLeaseTokenOutlivesFirstLeaseWindow checks the token an assignment
// handed out against the clock it will be presented on: still accepted after
// the first lease window (a renewed lease is alive then), refused past the
// hard cap, and advertised with that same cap in the mcp block.
func assertLeaseTokenOutlivesFirstLeaseWindow(t *testing.T, mcp *WSTaskMCP, assignedAt time.Time) {
	t.Helper()
	if mcp == nil || mcp.Token == "" {
		t.Fatal("the assignment carried no mcp block")
	}
	secret := []byte(leaseLifetimeSecret)
	for _, after := range []time.Duration{leaseTTL + time.Minute, leaseLifetimeHardCap - time.Minute} {
		if _, err := taskmcp.VerifyLeaseToken(secret, mcp.Token, assignedAt.Add(after)); err != nil {
			t.Errorf("%s after assignment the token is rejected (%v); the lease is renewed past its first %s window and the token is never re-sent", after, err, leaseTTL)
		}
	}
	if _, err := taskmcp.VerifyLeaseToken(secret, mcp.Token, assignedAt.Add(leaseLifetimeHardCap+time.Minute)); !errors.Is(err, taskmcp.ErrLeaseTokenExpired) {
		t.Errorf("%s after assignment: err = %v, want %v", leaseLifetimeHardCap+time.Minute, err, taskmcp.ErrLeaseTokenExpired)
	}
	claims, err := taskmcp.VerifyLeaseToken(secret, mcp.Token, assignedAt)
	if err != nil {
		t.Fatalf("the token does not verify at assignment: %v", err)
	}
	if want := claims.ExpiresAt.Format(time.RFC3339); mcp.ExpiresAt != want {
		t.Errorf("mcp.expires_at = %q, want the token's own expiry %q", mcp.ExpiresAt, want)
	}
}

func TestTaskAssignMCPTokenOutlivesFirstLeaseWindow(t *testing.T) {
	assign, assignedAt := leaseLifetimeAssignOverRelay(t)
	assertLeaseTokenOutlivesFirstLeaseWindow(t, assign.MCP, assignedAt)
}

func TestStandbyDispatchMCPTokenOutlivesFirstLeaseWindow(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	cfg.TaskMCP.RemoteEnabled = true
	cfg.Dashboard.PublicURL = "https://hive.example"
	s, _ := standbyAutoDispatchHarness(t, cfg)
	s.authToken = leaseLifetimeSecret

	msg, err := s.contributeHub.DispatchStandby("quality", "")
	if err != nil || msg == nil || msg.Type != "task_assign" {
		t.Fatalf("DispatchStandby = %#v, %v; want a task_assign", msg, err)
	}
	assertLeaseTokenOutlivesFirstLeaseWindow(t, msg.MCP, leaseLifetimeAssignedAt(t, s.contributeHub, msg.TaskID))
}

// leaseLifetimeTask is one task as the hub holds it mid-flight: who holds it,
// the assignment, and the lease token minted for it.
type leaseLifetimeTask struct {
	identity string
	assign   *WSTaskAssign
	token    string
}

// startLeaseLifetimeTask puts owner/repo#42 on the hub the way an assignment
// at assignedAt does - a connection holding it, the server-side lease, and a
// lease token expiring at tokenExpiry - and then renews the lease as the
// relay's progress report thirty seconds ago did. The hub reads the wall
// clock, so the stamps are aged rather than the test slept.
func startLeaseLifetimeTask(t *testing.T, s *Server, username string, assignedAt, tokenExpiry time.Time) leaseLifetimeTask {
	t.Helper()
	h := s.contributeHub
	task := leaseLifetimeTask{
		identity: username + "-id",
		assign:   &WSTaskAssign{TaskID: fmt.Sprintf("ct-owner/repo-42-%d", assignedAt.Unix()), Kind: "issue", Repo: "owner/repo", Number: 42, Title: "leased work"},
	}
	h.mu.Lock()
	h.connections[username] = &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: username, ContributorID: task.identity},
		currentTask:    task.assign,
		currentTaskGen: 2,
		taskAssignedAt: assignedAt,
	}
	h.mu.Unlock()
	if err := h.recordLeaseForKey(task.identity, task.assign.TaskID, task.assign.Repo, task.assign.Number, task.assign.identityKey(), "trusted", 2, assignedAt); err != nil {
		t.Fatalf("recordLeaseForKey: %v", err)
	}
	mcp := h.mintTaskMCPForAssignment(task.identity, task.assign, tokenExpiry, username)
	if mcp == nil || mcp.Token == "" {
		t.Fatal("no mcp block minted")
	}
	task.token = mcp.Token
	if err := h.renewLease(task.identity, task.assign.TaskID, time.Now().Add(-30*time.Second)); err != nil {
		t.Fatalf("renewLease: %v", err)
	}
	if left := task.leaseLeft(h); left <= 0 {
		t.Fatalf("setup: the renewed lease must be alive, has %s left", left)
	}
	return task
}

// leaseLeft is how much of the task's lease window remains; zero when the hub
// holds no lease for it.
func (task leaseLifetimeTask) leaseLeft(h *ContributeWSHub) time.Duration {
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l := h.leaseForLocked(task.identity, task.assign.TaskID)
	if l == nil {
		return 0
	}
	return time.Until(l.expiresAt).Round(time.Second)
}

// ageLease moves the lease's expiry back by d, as if that much wall-clock had
// passed with no progress report.
func (task leaseLifetimeTask) ageLease(t *testing.T, h *ContributeWSHub, d time.Duration) {
	t.Helper()
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	l := h.leaseForLocked(task.identity, task.assign.TaskID)
	if l == nil {
		t.Fatalf("no lease for %s", task.assign.TaskID)
	}
	l.expiresAt = l.expiresAt.Add(-d)
}

// The reported defect: a task assigned more than one lease window ago, whose
// relay is still reporting progress, was answered 403.
func TestContributeMCPRenewedLeaseKeepsItsTokenWorking(t *testing.T) {
	lifetime := leaseTokenLifetimeAtAssignment(t)
	for _, age := range []time.Duration{time.Minute, leaseTTL + time.Minute, 6 * time.Hour} {
		t.Run("assigned "+age.String()+" ago", func(t *testing.T) {
			s := newLeaseLifetimeServer(t)
			assignedAt := time.Now().Add(-age)
			task := startLeaseLifetimeTask(t, s, "alice", assignedAt, assignedAt.Add(lifetime))

			rec := leaseLifetimeCall(s, task.token)
			if rec.Code != http.StatusOK {
				t.Fatalf("task assigned %s ago still holds its lease (%s left) but the task MCP answers HTTP %d %s",
					age, task.leaseLeft(s.contributeHub), rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			var env struct {
				Data taskmcp.TaskContextData `json:"data"`
			}
			if err := json.Unmarshal([]byte(mcpResultText(t, rec.Body.Bytes())), &env); err != nil {
				t.Fatal(err)
			}
			if env.Data.Assignment.TaskID != task.assign.TaskID {
				t.Fatalf("assignment = %#v, want task %s", env.Data.Assignment, task.assign.TaskID)
			}
		})
	}
}

// The other half of the rule: a token still inside its own hard cap stops
// working the moment the lease it is bound to stops backing it.
func TestContributeMCPLeaseTokenDiesWithItsLease(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, s *Server, task leaseLifetimeTask)
	}{
		{"task released", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			s.contributeHub.revokeLease(task.identity, task.assign.TaskID)
		}},
		{"lease expired without renewal", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			task.ageLease(t, s.contributeHub, leaseTTL)
		}},
		{"expired lease pruned", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			task.ageLease(t, s.contributeHub, leaseTTL)
			if dropped := s.contributeHub.pruneExpiredLeases(time.Now()); dropped != 1 {
				t.Fatalf("pruneExpiredLeases dropped %d leases, want 1", dropped)
			}
		}},
		{"task reassigned to another contributor", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			s.contributeHub.revokeLease(task.identity, task.assign.TaskID)
			now := time.Now()
			next := startLeaseLifetimeTask(t, s, "bob", now, now.Add(leaseLifetimeHardCap))
			if rec := leaseLifetimeCall(s, next.token); rec.Code != http.StatusOK {
				t.Fatalf("the new holder's own token: status = %d, want 200", rec.Code)
			}
		}},
		{"lease recorded again for the same task", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			if err := s.contributeHub.recordLeaseForKey(task.identity, task.assign.TaskID, task.assign.Repo, task.assign.Number, task.assign.identityKey(), "trusted", 3, time.Now()); err != nil {
				t.Fatalf("recordLeaseForKey: %v", err)
			}
		}},
		{"token replaced by a newer one", func(t *testing.T, s *Server, task leaseLifetimeTask) {
			newer := s.contributeHub.mintTaskMCPForAssignment(task.identity, task.assign, time.Now().Add(leaseLifetimeHardCap), "alice")
			if newer == nil || newer.Token == "" || newer.Token == task.token {
				t.Fatal("no replacement token minted")
			}
			if rec := leaseLifetimeCall(s, newer.Token); rec.Code != http.StatusOK {
				t.Fatalf("the replacement token: status = %d, want 200", rec.Code)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newLeaseLifetimeServer(t)
			// Two hours in: well past the first lease window, well inside the cap.
			assignedAt := time.Now().Add(-2 * time.Hour)
			task := startLeaseLifetimeTask(t, s, "alice", assignedAt, assignedAt.Add(leaseLifetimeHardCap))
			if rec := leaseLifetimeCall(s, task.token); rec.Code != http.StatusOK {
				t.Fatalf("setup: the token must work while its lease is alive, status = %d", rec.Code)
			}

			tc.end(t, s, task)

			if rec := leaseLifetimeCall(s, task.token); rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: the token outlived its lease (%s left on the hub's lease for this task)",
					rec.Code, task.leaseLeft(s.contributeHub))
			}
		})
	}
}

// A lease answers only for the exact token it recorded. A twin the hub's own
// secret signed for the same lease under the same id, but with a later
// expiry, is refused: the cap cannot be stretched by re-signing.
func TestContributeMCPLeaseTokenIsMatchedByHash(t *testing.T) {
	s := newLeaseLifetimeServer(t)
	assignedAt := time.Now().Add(-2 * time.Hour)
	task := startLeaseLifetimeTask(t, s, "alice", assignedAt, assignedAt.Add(leaseLifetimeHardCap))
	secret := []byte(leaseLifetimeSecret)
	claims, err := taskmcp.VerifyLeaseToken(secret, task.token, time.Now())
	if err != nil {
		t.Fatalf("the leased token does not verify: %v", err)
	}
	claims.ExpiresAt = claims.ExpiresAt.Add(leaseLifetimeHardCap)
	twin, _, err := taskmcp.MintLeaseToken(secret, claims, time.Now())
	if err != nil || twin == task.token {
		t.Fatalf("mint twin: token changed = %v, err = %v", twin != task.token, err)
	}
	if rec := leaseLifetimeCall(s, twin); rec.Code != http.StatusForbidden {
		t.Fatalf("twin token with the lease's token id: status = %d, want 403", rec.Code)
	}
	if rec := leaseLifetimeCall(s, task.token); rec.Code != http.StatusOK {
		t.Fatalf("the recorded token: status = %d, want 200", rec.Code)
	}
}

// The cap holds even for a lease that is still being renewed.
func TestContributeMCPLeaseTokenRefusedPastItsHardCap(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want int
	}{
		{leaseLifetimeHardCap - time.Hour, http.StatusOK},
		{leaseLifetimeHardCap + time.Hour, http.StatusForbidden},
	} {
		t.Run("assigned "+tc.age.String()+" ago", func(t *testing.T) {
			s := newLeaseLifetimeServer(t)
			assignedAt := time.Now().Add(-tc.age)
			task := startLeaseLifetimeTask(t, s, "alice", assignedAt, assignedAt.Add(leaseLifetimeHardCap))
			if rec := leaseLifetimeCall(s, task.token); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s left on the lease)", rec.Code, tc.want, task.leaseLeft(s.contributeHub))
			}
		})
	}
}

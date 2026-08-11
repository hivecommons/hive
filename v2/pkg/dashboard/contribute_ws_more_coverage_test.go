package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func covK2Hub(t *testing.T) (*ContributeWSHub, *Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	hub := NewContributeWSHub(logger, s)
	return hub, s
}

// loadActivity / saveActivity / loadCompletedTasks / saveCompletedTasks read and
// write fixed /data consts absent/unwritable in the sandbox, so these exercise
// the no-file (early return) and write-error branches without touching real data.
func TestCovK2_ActivityDiskFuncs(t *testing.T) {
	hub, _ := covK2Hub(t)

	// No file present → early return (already exercised at construction, call again).
	hub.loadActivity()
	hub.loadCompletedTasks()

	// saveActivity with an empty slice: marshals then attempts a write.
	hub.saveActivity()
	hub.saveCompletedTasks()
}

func TestCovK2_AddActivityAndRecent(t *testing.T) {
	hub, _ := covK2Hub(t)

	// A normal activity entry.
	hub.addActivity("alice", "task_complete", "scanner", "claude", "sonnet", "repo#1")

	// joined/left dedup: a second identical joined within the debounce window is dropped.
	hub.addActivity("bob", "joined", "reviewer", "copilot", "gpt", "")
	hub.addActivity("bob", "joined", "reviewer", "copilot", "gpt", "")

	// A left action for the same user still records.
	hub.addActivity("bob", "left", "reviewer", "copilot", "gpt", "")

	recent := hub.RecentActivity()
	if len(recent) == 0 {
		t.Fatalf("expected some activity entries")
	}
	// RecentActivity returns a copy — mutating it must not affect the hub.
	first := recent[0].Username
	recent[0].Username = "MUTATED"
	if hub.RecentActivity()[0].Username != first {
		t.Fatalf("RecentActivity did not return an independent copy")
	}
}

func TestCovK2_AddActivityCap(t *testing.T) {
	hub, _ := covK2Hub(t)
	// Push beyond the cap; the ring buffer keeps only the tail.
	for i := 0; i < maxActivityEntries+10; i++ {
		hub.addActivity("u", "task_complete", "scanner", "claude", "sonnet", "t")
	}
	if got := len(hub.RecentActivity()); got != maxActivityEntries {
		t.Fatalf("expected activity capped at %d, got %d", maxActivityEntries, got)
	}
}

func TestCovK2_MarkTaskCompletedAndCooldown(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.markTaskCompleted("org/repo", 42, "https://github.com/org/repo/pull/1")
	if !hub.isTaskInCooldown("org/repo", 42) {
		t.Fatalf("expected task in cooldown after marking complete")
	}
	if hub.isTaskInCooldown("org/repo", 99) {
		t.Fatalf("unmarked task should not be in cooldown")
	}
}

// TestConditionalCooldownByPRURL covers kubestellar/hive#2393 item 7: a
// completion that reports a PR URL keeps the full week-long cooldown, while a
// completion with no PR (agent merely went idle) gets only the short no-PR
// cooldown, so an issue where nothing shipped is not locked out for a week.
func TestConditionalCooldownByPRURL(t *testing.T) {
	hub, _ := covK2Hub(t)

	// WITH a PR URL -> full completedTaskCooldownHours (168h).
	hub.markTaskCompleted("org/withpr", 1, "https://github.com/org/withpr/pull/1")
	if !hub.isTaskInCooldown("org/withpr", 1) {
		t.Fatalf("task with PR should be in cooldown immediately after completion")
	}
	// Age it just under the short no-PR window: still in cooldown because it
	// holds the full 168h cooldown, not the short one.
	hub.completedMu.Lock()
	hub.completedTasks["org/withpr#1"] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if !hub.isTaskInCooldown("org/withpr", 1) {
		t.Fatalf("task with PR should still be in cooldown after %dh (full cooldown is %dh)",
			completedNoPRCooldownHours+1, completedTaskCooldownHours)
	}
	// Aged past the full cooldown -> released.
	hub.completedMu.Lock()
	hub.completedTasks["org/withpr#1"] = time.Now().Add(-(completedTaskCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown("org/withpr", 1) {
		t.Fatalf("task with PR should be released after the full cooldown elapses")
	}

	// WITHOUT a PR URL -> short completedNoPRCooldownHours.
	hub.markTaskCompleted("org/nopr", 2, "")
	if !hub.isTaskInCooldown("org/nopr", 2) {
		t.Fatalf("no-PR task should be in a (short) cooldown right after completion")
	}
	// Aged just past the short no-PR window -> released, unlike the 168h default.
	hub.completedMu.Lock()
	hub.completedTasks["org/nopr#2"] = time.Now().Add(-(completedNoPRCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown("org/nopr", 2) {
		t.Fatalf("no-PR task should be released after only %dh, not locked for %dh",
			completedNoPRCooldownHours, completedTaskCooldownHours)
	}
}

// selectTask covers: nil status → nil, suspended → nil, and a real pick when the
// status carries an actionable issue.
func TestCovK2_SelectTask(t *testing.T) {
	hub, s := covK2Hub(t)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// No status yet → explicit hub_not_ready negative-ack (#2546).
	if msg := hub.selectTask(conn); msg == nil || msg.Reason != taskUnavailableHubNotReady {
		t.Fatalf("expected task_unavailable/%s with no status, got %+v", taskUnavailableHubNotReady, msg)
	}

	// Suspended contribute → explicit contribution_suspended negative-ack (#2546).
	s.deps.Config.Hub.ContributeSuspended = true
	if msg := hub.selectTask(conn); msg == nil || msg.Reason != taskUnavailableContributionSuspended {
		t.Fatalf("expected task_unavailable/%s when suspended, got %+v", taskUnavailableContributionSuspended, msg)
	}
	s.deps.Config.Hub.ContributeSuspended = false

	// Provide a status with one actionable issue.
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(7),
						"title":  "Fix the bug",
						"url":    "https://github.com/myorg/repo1/issues/7",
						"author": "someone",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()

	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected a task assignment for the actionable issue")
	}
	if msg.Type != "task_assign" || msg.Number != 7 {
		t.Fatalf("unexpected task message: %+v", msg)
	}

	// A second select for the same issue while it's the connection's current task
	// is skipped (activeIssues), and after marking it complete it's in cooldown.
	hub.markTaskCompleted("myorg/repo1", 7, "https://github.com/myorg/repo1/pull/9")
	conn.currentTask = nil
	// The sole candidate is now in cooldown → empty candidate set → no_matching_work (#2546).
	if msg := hub.selectTask(conn); msg == nil || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/%s (cooldown) but got %+v", taskUnavailableNoMatchingWork, msg)
	}
}

// TestCovK2_SelectTaskPrioritizesOwnWork covers the #2390 ordering default:
// among the eligible actionable candidates, one authored by the connected
// contributor is chosen before a fresh item authored by someone else — and when
// the contributor has no own-authored candidate, selection falls back to the
// previous first-eligible order.
func TestCovK2_SelectTaskPrioritizesOwnWork(t *testing.T) {
	hub, s := covK2Hub(t)
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	// A repo whose first (earliest) actionable item is someone else's, and whose
	// second is the contributor's own. Without prioritization the first-eligible
	// pick would be #10 (someone else); with #2390 it must be #20 (alice's own).
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(10),
						"title":  "Someone else's fresh issue",
						"url":    "https://github.com/myorg/repo1/issues/10",
						"author": "bob",
					},
					map[string]any{
						"number": float64(20),
						"title":  "Alice's own work",
						"url":    "https://github.com/myorg/repo1/issues/20",
						"author": "alice",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()

	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected a task assignment")
	}
	if msg.Number != 20 {
		t.Fatalf("expected own work (#20) to be prioritized, got #%d", msg.Number)
	}

	// Fallback: with no own-authored candidate, selection keeps the previous
	// first-eligible order (the earliest item, #10).
	conn2 := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	msg2 := hub.selectTask(conn2)
	if msg2 == nil {
		t.Fatalf("expected a task assignment for carol")
	}
	if msg2.Number != 10 {
		t.Fatalf("expected fallback to first-eligible (#10) with no own work, got #%d", msg2.Number)
	}
}

// HandleWS on a plain (non-websocket) request fails the upgrade with a 4xx.
func TestCovK2_HandleWSNonWebsocket(t *testing.T) {
	hub, _ := covK2Hub(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	hub.HandleWS(rec, req)
	if rec.Code < 400 {
		t.Fatalf("expected an error status for a non-websocket upgrade, got %d", rec.Code)
	}
}

// Audit F9 (CWE-770). The connection cap counted len(h.connections), but a
// connection is only added to that map AFTER it authenticates. So the cap
// bounded exactly the clients that had already proven who they were, and left
// the pre-auth window — one goroutine, one socket and a read buffer for a full
// wsAuthTimeout each — completely unbounded.
//
// An unauthenticated client could therefore hold arbitrarily many sockets while
// the only parties actually limited were legitimate contributors. Sockets must
// occupy a slot from the moment they are upgraded.
func TestF9_UnauthenticatedConnectionsCountTowardCap(t *testing.T) {
	hub, _ := covK2Hub(t)
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Open sockets and never authenticate. Each should hold a slot.
	var open []*websocket.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for i := 0; i < maxWSConnections; i++ {
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial %d (below the cap) failed: %v", i, err)
		}
		open = append(open, c)
	}

	// Positive control: the pre-auth sockets must actually be tracked. If they
	// are not, the assertion below would be testing nothing.
	if got := hub.pendingConns.Load(); got == 0 {
		t.Fatalf("test is vacuous: %d unauthenticated sockets are open but pendingConns is 0", len(open))
	}
	// None of them authenticated, so the authenticated map must still be empty.
	hub.mu.RLock()
	authed := len(hub.connections)
	hub.mu.RUnlock()
	if authed != 0 {
		t.Fatalf("no connection authenticated, but h.connections holds %d", authed)
	}

	// The next dial is over the cap and must be refused.
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		c.Close()
		t.Fatal("F9: a connection beyond the cap was accepted — unauthenticated sockets do not count, so the limit only ever restrained authenticated contributors")
	}
	if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("rejected with %d, want 503", resp.StatusCode)
	}
}

// A socket that goes away must give its slot back, or the cap ratchets down to
// zero over time and the endpoint dies on its own.
func TestF9_ClosedPreAuthConnectionReleasesItsSlot(t *testing.T) {
	hub, _ := covK2Hub(t)
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleWS))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hub.pendingConns.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.pendingConns.Load() == 0 {
		t.Fatal("test is vacuous: the open socket was never counted as pending")
	}
	c.Close()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.pendingConns.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("F9: closed pre-auth connection never released its slot (pendingConns=%d) — the cap would ratchet down permanently", hub.pendingConns.Load())
}

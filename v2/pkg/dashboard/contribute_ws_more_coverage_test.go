package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	hub.markTaskCompleted("org/repo", 42)
	if !hub.isTaskInCooldown("org/repo", 42) {
		t.Fatalf("expected task in cooldown after marking complete")
	}
	if hub.isTaskInCooldown("org/repo", 99) {
		t.Fatalf("unmarked task should not be in cooldown")
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

	// No status yet → nil.
	if msg := hub.selectTask(conn); msg != nil {
		t.Fatalf("expected nil task with no status")
	}

	// Suspended contribute → nil.
	s.deps.Config.Hub.ContributeSuspended = true
	if msg := hub.selectTask(conn); msg != nil {
		t.Fatalf("expected nil task when contribute suspended")
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
	hub.markTaskCompleted("myorg/repo1", 7)
	conn.currentTask = nil
	if msg := hub.selectTask(conn); msg != nil {
		t.Fatalf("expected nil task (cooldown) but got %+v", msg)
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

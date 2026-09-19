package dashboard

// handleRestart's SUCCESS tail (api.go) had no coverage: every existing
// restart test drives the handler into the AgentMgr.Restart error branch
// (unknown agent, or a live relaunch leg that fails hermetically), so the
// audit log line, refreshAndPersist, the 200 body, and — critically — the
// #7363 kickCancelled flag were all unpinned. That flag is the operator's
// only signal that the restart cancelled a pending kick instead of silently
// replaying it into the relaunched CLI; a regression here reverts the
// operator-facing half of #7363 without failing any test.
//
// The hermetic recipe mirrors resumableServer (agent_control_error_paths_test.go)
// plus the fake-tmux-on-PATH pattern from pkg/agent's
// terminal_tmux_hermetic_test.go:
//   - the agent is CONFIG-seeded paused, so Manager.Restart returns nil right
//     after ensureTmuxSession, never reaching the mint/launch leg, and
//   - a fake tmux answering exit 0 makes has-session report the session as
//     already present (and absorbs the kill-session), so no real tmux server
//     is touched.

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// installRestartFakeTmux puts a fake tmux first on PATH that exits 0 for every
// invocation: has-session reports "exists" (so ensureTmuxSession is a no-op)
// and kill-session is absorbed. UID-isolated managers route every tmux call
// through su-exec, so a fake su-exec that drops the userspec and execs the
// rest of its argv is installed alongside — the chain still lands on the fake
// tmux. Nothing here touches a real tmux server or changes users.
func installRestartFakeTmux(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "su-exec"), []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// restartableServer builds an apiServer whose "scanner" carries a
// config-seeded pause: NewManager restores cfg.Paused as Paused=true with
// State at StateStopped. Restart on that shape takes the paused early return
// after ensureTmuxSession — the one restart with no mint and no relaunch leg —
// so the handler's success tail runs hermetically.
func restartableServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	installRestartFakeTmux(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	deps := testDeps(t)
	ac := deps.Config.Agents["scanner"]
	ac.Paused = true
	deps.Config.Agents["scanner"] = ac
	deps.AgentMgr = agent.NewManager(deps.Config.Agents, logger, agent.ProjectContext{})
	s := NewServer(0, logger)
	s.RegisterAPI(deps)
	return s, deps
}

// TestHandleRestartSuccessTailReportsKickCancelled pins the #7363 contract:
// when the restart teardown cancels a PENDING kick dispatch, the 200 body
// must carry kickCancelled:true so the operator knows the interrupted prompt
// will NOT be replayed into the relaunched CLI.
func TestHandleRestartSuccessTailReportsKickCancelled(t *testing.T) {
	s, deps := restartableServer(t)

	// A pending dispatch, exactly what SendKickAsync leaves while its
	// delivery goroutine waits for the CLI prompt. The restart teardown
	// (invalidateKicksOnRestartLocked) must fail it with a "cancelled:" error.
	deps.AgentMgr.RecordKickDispatchForTest(agent.KickDispatch{
		Agent:    "scanner",
		Phase:    agent.KickPhasePending,
		QueuedAt: time.Now(),
	})

	rec := doPost(s, "/api/restart/scanner", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("restart of paused stopped agent = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("restart reported ok=false: %v", body)
	}
	if status, _ := body["status"].(string); status != "restarted" {
		t.Errorf("status = %q, want %q", status, "restarted")
	}
	if cancelled, _ := body["kickCancelled"].(bool); !cancelled {
		t.Errorf("restart that cancelled a pending kick must report kickCancelled:true (#7363): %v", body)
	}

	// The registry itself must agree: the dispatch is failed with the
	// restart-cancellation reason, not still pending.
	d, ok := deps.AgentMgr.KickDispatchState("scanner")
	if !ok {
		t.Fatal("kick dispatch vanished from the registry")
	}
	if d.Phase != agent.KickPhaseFailed {
		t.Errorf("dispatch phase = %q, want %q", d.Phase, agent.KickPhaseFailed)
	}
}

// TestHandleRestartSuccessTailWithoutPendingKick pins the flag's ABSENCE: a
// clean restart (no pending kick) answers 200 restarted with NO kickCancelled
// key. If the handler ever set the flag unconditionally, operators would read
// every restart as having destroyed a kick.
func TestHandleRestartSuccessTailWithoutPendingKick(t *testing.T) {
	s, _ := restartableServer(t)

	rec := doPost(s, "/api/restart/scanner", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("restart of paused stopped agent = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("restart reported ok=false: %v", body)
	}
	if status, _ := body["status"].(string); status != "restarted" {
		t.Errorf("status = %q, want %q", status, "restarted")
	}
	if _, present := body["kickCancelled"]; present {
		t.Errorf("kickCancelled must be omitted when no kick was pending: %v", body)
	}
}

// TestHandleRestartSuccessTailPreservesPausedState guards the shape this
// suite depends on end to end: a restart of a paused agent must leave it
// paused (Manager.Restart's paused early return), never silently resume it.
// If that early return moved or the restart began clearing Paused, both
// success-tail tests above would be exercising a different (launch-bearing)
// path without noticing.
func TestHandleRestartSuccessTailPreservesPausedState(t *testing.T) {
	s, deps := restartableServer(t)

	if rec := doPost(s, "/api/restart/scanner", nil); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil || proc == nil {
		t.Fatalf("GetStatus after restart: %v", err)
	}
	if !proc.Paused {
		t.Error("restart of a paused agent must preserve the pause")
	}
	if proc.State != agent.StatePaused {
		t.Errorf("state = %q, want %q", proc.State, agent.StatePaused)
	}
}

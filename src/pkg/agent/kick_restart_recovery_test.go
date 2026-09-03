package agent

// Tests for SendKick's crash-recovery branch and for RestartThenSendKick.
// The contract under pin: a kick aimed at a pane whose CLI has died (bare
// shell — no CLI marker) must NOT be typed into bash; SendKick restarts the
// CLI first and only then delivers, so the message reaches an agent, not a
// shell prompt (observed live: "-bash: NEVER: command not found").
//
// These are also the end-to-end regression tests for the Restart launch-race
// guard: with the pre-fix `State == StateRunning` form of that guard, the
// restart-before-kick path killed the CLI and then silently declined to
// relaunch it, and both tests time out waiting for a CLI that never comes.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// forceFastPaneShell pins the test tmux server's default-shell to /bin/sh so
// freshly created panes accept input immediately. A developer's login shell
// (zsh + prompt frameworks) can take seconds to initialize and DISCARDS
// keystrokes typed during init, silently swallowing the launch line these
// tests depend on; CI's bash never does, so without this the tests would be
// host-dependent. The anchor session keeps the per-process tmux server alive
// so the global option sticks; the option is unset again on cleanup.
func forceFastPaneShell(t *testing.T) {
	t.Helper()
	const anchor = "hive-fastshell-anchor"
	_ = testTmuxCommand("new-session", "-d", "-s", anchor).Run() // ok if it exists
	t.Cleanup(func() {
		_ = testTmuxCommand("set-option", "-gu", "default-shell").Run()
		_ = testTmuxCommand("kill-session", "-t", anchor).Run()
	})
	if err := testTmuxCommand("set-option", "-g", "default-shell", "/bin/sh").Run(); err != nil {
		t.Skipf("cannot pin tmux default-shell: %v", err)
	}
}

// quiesceThenMarkReady arms a helper goroutine for a test whose flow is
// "relaunch, wait for readiness, deliver a kick" in one call. As soon as the
// relaunch completes (HasLaunched + cancel set, both written under m.mu by
// launchInTmux), it:
//
//  1. cancels the launch's pollTmuxOutputForAgent goroutine. Kick delivery
//     and a live poller race on agent bookkeeping fields by design (see the
//     comment on TestSendKick_DeliversToReadyPane) — a coverage test must not
//     exercise that interleaving under -race. The poller's KickRefused read
//     only happens from its SECOND 3s tick onward (the first iteration exits
//     at prevLines == nil), and the cancellation lands well inside the first
//     tick, so no conflicting access ever runs;
//  2. waits out one poll interval so the poller has observed the cancel;
//  3. renders "goose is ready" into the real pane, which satisfies both
//     waitForCLIReadyForAgent (CLI marker) and waitForInputPromptForAgent
//     (input prompt), letting the kick proceed against a quiet pane.
//
// The returned wait func must be deferred (after cleanupAgent registration)
// so the goroutine is drained before the test's env restores run.
func quiesceThenMarkReady(t *testing.T, m *Manager, name string) (wait func()) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.Lock()
			agent, ok := m.agents[name]
			var cancel context.CancelFunc
			session := ""
			if ok && agent.HasLaunched && agent.cancel != nil {
				cancel = agent.cancel
				session = agent.tmuxSession
			}
			m.mu.Unlock()
			if cancel == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			cancel()
			// One poll interval (3s) with margin, so the poller has observed
			// ctx.Done before anything below makes the pane interesting.
			time.Sleep(4 * time.Second)
			_ = testTmuxCommand("send-keys", "-t", session, "-l", ": goose is ready").Run()
			_ = testTmuxCommand("send-keys", "-t", session, "Enter").Run()
			return
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
	}
}

// TestSendKick_RestartsCrashedCLIThenDelivers: the pane exists but shows a
// bare shell (the CLI crashed), so SendKick must take the restart branch —
// relaunch the CLI, wait for it to become ready, and still deliver the kick.
func TestSendKick_RestartsCrashedCLIThenDelivers(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	forceFastPaneShell(t)

	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["worker"]
	m.mu.RUnlock()

	// A real session whose pane is a bare shell: session exists, CLI does not.
	newRawTmuxSession(t, agent.tmuxSession)
	m.mu.Lock()
	agent.State = StateRunning
	m.mu.Unlock()

	wait := quiesceThenMarkReady(t, m, "worker")
	defer wait()
	defer cleanupAgent(t, m, "worker")

	if err := m.SendKick("worker", "recover and continue the sweep"); err != nil {
		t.Fatalf("SendKick on crashed CLI: %v", err)
	}

	m.mu.RLock()
	restarts := agent.RestartCount
	relaunched := agent.HasLaunched
	lastMsg := agent.LastKickMessage
	m.mu.RUnlock()
	if restarts != 1 {
		t.Errorf("RestartCount = %d, want 1 (crashed CLI must be restarted before the kick)", restarts)
	}
	if !relaunched {
		t.Error("restart did not relaunch the CLI — the kick was delivered into a dead pane")
	}
	if lastMsg != "recover and continue the sweep" {
		t.Errorf("LastKickMessage = %q — the kick was lost in the restart", lastMsg)
	}
}

// TestRestartThenSendKick_CleanSlateThenDelivers pins the composed contract:
// a clean-slate restart (no bootstrap override), a readiness wait, and a
// prompt-waited SendKick delivery — the reliable alternative to
// RestartWithBootstrap's fragile $(cat file) shell expansion.
func TestRestartThenSendKick_CleanSlateThenDelivers(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	forceFastPaneShell(t)

	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})

	wait := quiesceThenMarkReady(t, m, "worker")
	defer wait()
	defer cleanupAgent(t, m, "worker")

	if err := m.RestartThenSendKick(context.Background(), "worker", "fresh context, take the next issue"); err != nil {
		t.Fatalf("RestartThenSendKick: %v", err)
	}

	m.mu.RLock()
	agent := m.agents["worker"]
	restarts := agent.RestartCount
	lastMsg := agent.LastKickMessage
	m.mu.RUnlock()
	if restarts != 1 {
		t.Errorf("RestartCount = %d, want 1", restarts)
	}
	if lastMsg != "fresh context, take the next issue" {
		t.Errorf("LastKickMessage = %q — the kick must be delivered after the restart", lastMsg)
	}
}

// TestRestartThenSendKick_UnknownAgentSurfacesRestartError: the restart leg's
// failure must be surfaced (wrapped), not swallowed into a silent no-kick.
func TestRestartThenSendKick_UnknownAgentSurfacesRestartError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.RestartThenSendKick(context.Background(), "ghost", "msg")
	if err == nil {
		t.Fatal("RestartThenSendKick on unknown agent returned nil, want restart error")
	}
}

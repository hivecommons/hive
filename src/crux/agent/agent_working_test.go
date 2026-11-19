package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// TestWaitForInputPrompt_WorkingGate exercises the real readiness path
// (waitForInputPromptForAgent) through the terminal seam, proving the working
// check is wired to the VISIBLE pane. This is the call-site regression guard
// for #7085 and the surface the scrollback mutation must break.
func TestWaitForInputPrompt_WorkingGate(t *testing.T) {
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = 400 * time.Millisecond
	inputPromptPollInterval = 20 * time.Millisecond
	defer func() {
		inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
	}()

	newAgent := func() (*Manager, *AgentProcess) {
		m := NewManager(
			map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
			discardLogger(), ProjectContext{})
		m.mu.Lock()
		agent := m.agents["scanner"]
		m.mu.Unlock()
		return m, agent
	}

	busyVisible := " ◉ Working · 42.6 KiB esc interrupt   Claude Fable 5\n❯"
	idleVisible := "────────\n❯\n────────\n idle   Claude Fable 5"

	// Busy: the VISIBLE pane shows a working indicator, so the gate must
	// never report ready — it must ride inputPromptTimeout out to false.
	m, agent := newAgent()
	ft := termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string { return busyVisible }
	ft.capturePane = func(*AgentProcess) string { return busyVisible }
	if m.waitForInputPromptForAgent(agent) {
		t.Error("busy pane: waitForInputPromptForAgent returned ready; " +
			"want not ready (#7085)")
	}

	// Idle: a genuine idle prompt must still read as ready (no regression).
	m, agent = newAgent()
	ft = termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string { return idleVisible }
	ft.capturePane = func(*AgentProcess) string { return idleVisible }
	if !m.waitForInputPromptForAgent(agent) {
		t.Error("idle pane: waitForInputPromptForAgent returned not ready; " +
			"want ready")
	}

	// Working marker ONLY in scrollback: the visible pane is idle, but the
	// full capture (with scrollback) still carries a finished task's marker.
	// The gate consults the VISIBLE pane for working, so it must read READY.
	// If the check ever searches scrollback, this agent is never kicked again.
	m, agent = newAgent()
	ft = termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string { return idleVisible }
	ft.capturePane = func(*AgentProcess) string { return busyVisible + "\n" + idleVisible }
	if !m.waitForInputPromptForAgent(agent) {
		t.Error("working marker in scrollback only: returned not ready; " +
			"want ready (gate must use the visible pane, not scrollback)")
	}
}

// A prompt wait that times out because the agent is still mid-turn must not
// log the WARN pane dump: the slog feed files that line as agent_pane_timeout,
// so every re-kick that outwaited a long turn self-reported (#17993). A pane
// that never reaches a prompt for any other reason still dumps at WARN.
func TestWaitForInputPrompt_TimeoutWhileWorkingIsNotWarned(t *testing.T) {
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = 200 * time.Millisecond
	inputPromptPollInterval = 10 * time.Millisecond
	defer func() {
		inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
	}()

	run := func(pane string) string {
		t.Helper()
		var logBuf bytes.Buffer
		m := NewManager(
			map[string]config.AgentConfig{"quality": {Backend: "claude"}},
			slog.New(slog.NewTextHandler(&logBuf, nil)), ProjectContext{})
		m.mu.Lock()
		agent := m.agents["quality"]
		m.mu.Unlock()
		ft := termSeams(m)
		ft.captureVisiblePane = func(*AgentProcess) string { return pane }
		ft.capturePane = func(*AgentProcess) string { return pane }
		if m.waitForInputPromptForAgent(agent) {
			t.Fatalf("pane %q: returned ready; want not ready", pane)
		}
		return logBuf.String()
	}

	busy := run(" ◉ Working · 42.6 KiB esc interrupt   Claude Fable 5\n❯")
	if strings.Contains(busy, "prompt timeout — dumping pane") {
		t.Errorf("mid-turn timeout logged a WARN pane dump; want an INFO line only:\n%s", busy)
	}
	if !strings.Contains(busy, "prompt wait ended: agent still mid-turn") {
		t.Errorf("mid-turn timeout did not log the mid-turn INFO line:\n%s", busy)
	}

	stuck := run("some unrecognised screen with no prompt")
	if !strings.Contains(stuck, "prompt timeout — dumping pane") {
		t.Errorf("non-working timeout must still dump the pane at WARN:\n%s", stuck)
	}
}

// Bob-Shell 2.x keeps the "❯" input box painted while a response streams and
// signals busy only with "⠇ Processing… (Enter to steer, Tab to queue)". The
// busy pane must read as working (delivery gate) and NOT as a finished turn
// (turn-end classification / kick log archive); the idle pane must do the
// opposite. Captures are from the live hive in #15864.
func TestPaneShowsAgentWorking_BobProcessing(t *testing.T) {
	busyBob := `• All referenced issues are still open. Now let me look at the current state of the code.
 Execute Command (completed)
 Command: ls /tmp/hive/ 2>/dev/null || echo "no hive dir"
 bin
 ⠇ Processing… (Enter to steer, Tab to queue)
───────────────────────────────────────────────────────────────────────
 ❯   Build Anything, @ for context, / for commands, $ for skills
───────────────────────────────────────────────────────────────────────
 Agent Mode (auto-approve) · 22.6k / 270.0k (8%) · 0.380 🅞`
	if !paneShowsAgentWorking(busyBob) {
		t.Error("a bob pane showing Processing… (Enter to steer) must read as working (#15864)")
	}
	if paneShowsTurnEnded(busyBob) {
		t.Error("a bob pane showing Processing… (Enter to steer) must not read as turn ended (#15864)")
	}

	idleBob := `  ### Key Recommendations This Cycle
  1. PR #179 — Reviewer should clarify or dismiss the stale CHANGES_REQUESTED.
───────────────────────────────────────────────────────────────────────
 ❯   Build Anything, @ for context, / for commands, $ for skills
───────────────────────────────────────────────────────────────────────
 Agent Mode (auto-approve) · 31.9k / 270.0k (12%) · 0.984 🅞`
	if paneShowsAgentWorking(idleBob) {
		t.Error("an idle bob pane must not read as working")
	}
	if !paneShowsTurnEnded(idleBob) {
		t.Error("an idle bob pane must read as turn ended")
	}
}

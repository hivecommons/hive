package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestPaneShowsAgentWorking is the regression guard for #7085: modern TUI
// backends (Claude Code / Claude Fable) keep the "❯" input box rendered while
// the agent is actively working, so paneShowsInputPrompt's "❯" check passes on
// a busy agent and a kick would Ctrl+C + /clear its in-flight work. The gate is
// safety-critical in BOTH directions — too loose keeps destroying live work,
// too tight wedges a genuinely idle agent forever — so the idle and
// scrollback-only cases below are hard regression guards, not nice-to-haves.
func TestPaneShowsAgentWorking(t *testing.T) {
	// The exact mid-task capture quoted in #7085: "❯" and the working
	// indicators ("◉ Working", "esc interrupt") are present at the same time.
	busyClaudeFable := ` ● Read shell output Waiting up to 420 seconds for command output       2m 35s
 /data/agents/scanner [⎇ deps-lucide*+%]              Session: 430 AIC used
────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 ◉ Working · 42.6 KiB esc interrupt                            Claude Fable 5`

	// A genuine idle prompt: the "❯" input box is drawn but there is no
	// working indicator anywhere in the visible pane. Must read as ready.
	idlePrompt := `────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 /data/agents/scanner [⎇ deps-lucide*+%]              Session: 430 AIC used
                                                              Claude Fable 5`

	// A COMPLETED task followed by a fresh idle prompt. The working marker
	// ("◉ Working ... esc interrupt") is from the finished turn and would only
	// appear here if a caller passed scrollback instead of the visible pane.
	// The visible region is idle and must read as ready — otherwise the agent
	// is never kicked again and the hive stalls silently.
	idleVisibleWorkingInScrollback := `────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 done · 42.6 KiB                                              Claude Fable 5`

	escInterruptOnly := `Processing your request…
 ◉ Working · 12.1 KiB esc interrupt                            Claude Fable 5`

	spinnerOnly := ` ◉ Working · 3.0 KiB                                           Claude Fable 5
❯`

	// goose and codex render no working indicator at all; their idle panes
	// must never be misclassified as working.
	gooseIdle := "goose is ready\n> "
	codexIdle := "› \nPress enter to continue"

	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"empty pane is not working", "", false},
		{"busy claude fable capture (#7085) is working", busyClaudeFable, true},
		{"genuine idle prompt is not working", idlePrompt, false},
		{"visible idle prompt, no working marker", idleVisibleWorkingInScrollback, false},
		{"esc interrupt footer is working", escInterruptOnly, true},
		{"working spinner is working", spinnerOnly, true},
		{"goose idle is not working", gooseIdle, false},
		{"codex idle is not working", codexIdle, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsAgentWorking(tc.pane); got != tc.want {
				t.Errorf("paneShowsAgentWorking(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestPaneShowsAgentWorking_ReadinessContract ties the working check back to the
// readiness decision the way waitForInputPromptForAgent uses it: a pane is
// READY only when paneShowsInputPrompt sees a prompt AND the visible pane is
// not actively working. The scrollback-only case is the critical one — the
// readiness path feeds the VISIBLE pane to paneShowsAgentWorking, so a lingering
// marker in history must not block a genuinely idle agent.
func TestPaneShowsAgentWorking_ReadinessContract(t *testing.T) {
	// ready(visible) mirrors the gate in waitForInputPromptForAgent: the
	// visible pane decides "working", the (full) capture decides "has prompt".
	ready := func(visible, fullCapture string) bool {
		if paneShowsAgentWorking(visible) {
			return false
		}
		return paneShowsInputPrompt(fullCapture)
	}

	busy := ` ◉ Working · 42.6 KiB esc interrupt                            Claude Fable 5
❯`
	if ready(busy, busy) {
		t.Errorf("busy pane classified READY; must be not ready (#7085)")
	}

	idle := `────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────
 idle                                                          Claude Fable 5`
	if !ready(idle, idle) {
		t.Errorf("idle pane classified NOT ready; must be ready (no regression)")
	}

	// The working marker is only in scrollback; the VISIBLE pane is idle.
	visibleIdle := "────────────\n❯\n────────────\n idle   Claude Fable 5"
	fullWithScrollback := " ◉ Working · 9 KiB esc interrupt\n" + visibleIdle
	if !ready(visibleIdle, fullWithScrollback) {
		t.Errorf("idle visible pane with working marker in scrollback classified " +
			"NOT ready; must be ready or the agent is never kicked again")
	}
}

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

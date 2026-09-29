package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestWaitForInputPromptRecordsConsecutiveTimeouts is the regression guard for
// #9445: a CLI wedged mid-turn shows Working/spinner forever with no error, so
// waitForInputPromptForAgentUnless times out on every kick attempt while
// state=running/busy=working and deepHealth stall_detection stay green
// throughout. The wait loop is the only place this failure is observed, so it
// must be the place that counts it — three consecutive timeouts here is what
// the dashboard escalates on.
func TestWaitForInputPromptRecordsConsecutiveTimeouts(t *testing.T) {
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = 60 * time.Millisecond
	inputPromptPollInterval = 10 * time.Millisecond
	defer func() {
		inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
	}()

	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["scanner"]
	m.mu.Unlock()

	// A pane that never reaches an input prompt: the wedge this issue
	// describes ("Working" with no error, forever).
	busyVisible := " ◉ Working · 40.9 KiB esc interrupt"
	ft := termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string { return busyVisible }
	ft.capturePane = func(*AgentProcess) string { return busyVisible }

	for i := 1; i <= 3; i++ {
		if m.waitForInputPromptForAgent(agent) {
			t.Fatalf("attempt %d: wedged pane reported ready; want not ready", i)
		}
		agent.paneMu.RLock()
		got := agent.KickDeliveryTimeouts
		last := agent.LastKickDeliveryTimeoutAt
		agent.paneMu.RUnlock()
		if got != i {
			t.Fatalf("attempt %d: KickDeliveryTimeouts = %d, want %d", i, got, i)
		}
		if last.IsZero() {
			t.Fatalf("attempt %d: LastKickDeliveryTimeoutAt not set", i)
		}
	}

	// The pane clears (a kick or an operator interrupt succeeded): the next
	// wait reaches a real input prompt, and the streak must reset to zero —
	// an agent that is merely between long turns must never accumulate this.
	idleVisible := "────────\n❯\n────────\n idle"
	ft.captureVisiblePane = func(*AgentProcess) string { return idleVisible }
	ft.capturePane = func(*AgentProcess) string { return idleVisible }

	if !m.waitForInputPromptForAgent(agent) {
		t.Fatal("cleared pane: waitForInputPromptForAgent returned not ready; want ready")
	}
	agent.paneMu.RLock()
	got := agent.KickDeliveryTimeouts
	last := agent.LastKickDeliveryTimeoutAt
	agent.paneMu.RUnlock()
	if got != 0 {
		t.Fatalf("after recovery: KickDeliveryTimeouts = %d, want 0", got)
	}
	if !last.IsZero() {
		t.Fatalf("after recovery: LastKickDeliveryTimeoutAt = %v, want zero", last)
	}
}

// TestWaitForInputPromptTimeoutDoesNotCountAbortedWaits confirms an aborted
// wait (a restart invalidating a pending kick, #7363) does not inflate the
// wedge counter: only a genuine inputPromptTimeout exhaustion is evidence of a
// stuck CLI, never a wait this manager itself chose to cut short.
func TestWaitForInputPromptTimeoutDoesNotCountAbortedWaits(t *testing.T) {
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = 2 * time.Second
	inputPromptPollInterval = 10 * time.Millisecond
	defer func() {
		inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
	}()

	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["scanner"]
	m.mu.Unlock()

	busyVisible := " ◉ Working · 40.9 KiB esc interrupt"
	ft := termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string { return busyVisible }
	ft.capturePane = func(*AgentProcess) string { return busyVisible }

	if m.waitForInputPromptForAgentUnless(agent, func() bool { return true }) {
		t.Fatal("aborted wait reported ready; want not ready")
	}
	agent.paneMu.RLock()
	got := agent.KickDeliveryTimeouts
	agent.paneMu.RUnlock()
	if got != 0 {
		t.Fatalf("aborted wait: KickDeliveryTimeouts = %d, want 0 (not a genuine timeout)", got)
	}
}

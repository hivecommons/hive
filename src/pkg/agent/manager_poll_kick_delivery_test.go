package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Delivering a kick is not instantaneous: deliverKickLocked types the message
// as 400-rune chunks, so a 37KB governor kick holds the pane for ~100s. While
// it does, the CLI's idle chrome is scrolled out of the captured pane — which
// is indistinguishable, to a predicate that only looks for the ABSENCE of that
// chrome, from a hung CLI.
//
// Observed live on the scanner agent (#7169):
//
//	11:36:14  audit: governor kicking agent   (37,564 chars)
//	11:37:22  copilot hung with no CLI prompt, running diagnostic
//	11:37:23  tmux session created            <- pane replaced mid-type
//	11:37:45  tmux send-keys failed ... x11   <- rest of the kick went nowhere
//	11:37:58  audit: agent restarting         restart_count=4
//
// interruptions_total had reached 5, so five consecutive governor kicks had
// been destroyed by the detector that is supposed to rescue the agent.
//
// These tests pin the guard from both sides: the detector must stay silent
// during delivery, and must still fire when no delivery is in flight.

// hangDetectorFired reports whether pollTmuxOutputForAgent's copilot-hang
// branch ran. That branch stamps lastTokenRestart immediately before launching
// the diagnostic, so an advanced stamp is the observable signal without having
// to race the diagnostic goroutine itself.
func hangDetectorFired(agent *AgentProcess, before time.Time) bool {
	return agent.lastTokenRestart.After(before)
}

// newHungCopilotAgent builds a copilot agent parked in the exact state the
// hang detector keys on: up well past expiredTokenHangTimeoutSec, cooldown
// long expired, and a pane with no CLI-ready indicator.
func newHungCopilotAgent(t *testing.T, name, session string) (*Manager, *AgentProcess) {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{name: {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents[name]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.lastTokenRestart = time.Now().Add(-10 * time.Minute)
	old := time.Now().Add(-5 * time.Minute)
	agent.StartedAt = &old
	return m, agent
}

// TestPollCopilotHang_SuppressedWhileKickDelivering is the regression: a pane
// with no CLI chrome must NOT trigger the diagnostic while a kick is being
// typed into it.
func TestPollCopilotHang_SuppressedWhileKickDelivering(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m, agent := newHungCopilotAgent(t, "kdsuppress", "hive-kdsuppress")
	defer cleanupAgent(t, m, "kdsuppress")

	// A pane mid-delivery: partially typed kick text, no idle prompt visible.
	paneInject(t, "hive-kdsuppress", "  8. git commit -s -m \"[scanner] fix: <short description>\"")

	agent.kickDelivering.Store(true)
	before := agent.lastTokenRestart

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)

	if hangDetectorFired(agent, before) {
		t.Fatal("copilot-hang diagnostic fired while a kick was being delivered; " +
			"it would have recreated the tmux session and destroyed the in-flight kick")
	}
}

// TestPollCopilotHang_FiresWhenNoKickInFlight is the control. Without it the
// test above would still pass if the detector were simply broken, and the
// guard would be indistinguishable from a regression.
func TestPollCopilotHang_FiresWhenNoKickInFlight(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m, agent := newHungCopilotAgent(t, "kdfires", "hive-kdfires")
	defer cleanupAgent(t, m, "kdfires")

	paneInject(t, "hive-kdfires", "still loading please wait")

	if agent.kickDelivering.Load() {
		t.Fatal("a freshly built agent reports a kick in flight")
	}
	before := agent.lastTokenRestart

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)

	if !hangDetectorFired(agent, before) {
		t.Fatal("copilot-hang diagnostic did not fire for a genuinely hung agent; " +
			"the kickDelivering guard has disabled the detector outright")
	}
}

// TestDeliverKickLocked_ClearsDeliveryFlag: the flag must be released on the
// way out, including when delivery panics partway through. A flag that latched
// on would permanently disable the hang detector for that agent — trading a
// destroyed kick for an agent that can never be recovered.
func TestDeliverKickLocked_ClearsDeliveryFlag(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	session := "hive-kdflag"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"kdflag": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	defer cleanupAgent(t, m, "kdflag")
	m.mu.RLock()
	agent := m.agents["kdflag"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	m.mu.Lock()
	m.deliverKickLocked(agent, "audit the repo", "send-kick")
	m.mu.Unlock()

	if agent.kickDelivering.Load() {
		t.Fatal("kickDelivering still set after deliverKickLocked returned; " +
			"the hang detector is now permanently suppressed for this agent")
	}
}

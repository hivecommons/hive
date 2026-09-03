package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestCheckAndRestartCrashedAgents_BarePaneRestarts: a running agent with a
// live tmux session whose pane is a bare shell (no CLI marker) and whose uptime
// is past the boot grace period is declared crashed and restarted.
func TestCheckAndRestartCrashedAgents_BarePaneRestarts(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	// Create a live session with a bare shell (no CLI marker).
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()

	old := time.Now().Add(-2 * time.Minute) // past cliBootGraceSeconds
	m.mu.Lock()
	agent.State = StateRunning
	agent.StartedAt = &old
	m.mu.Unlock()

	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	defer cleanupAgent(t, m, "cxa")
	if len(restarted) != 1 {
		t.Errorf("expected bare-pane agent to be restarted, got %v", restarted)
	}
}

// TestCheckAndRestartCrashedAgents_BootGrace: a bare pane within the boot grace
// window is NOT restarted.
func TestCheckAndRestartCrashedAgents_BootGrace(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()

	recent := time.Now() // within grace
	m.mu.Lock()
	agent.State = StateRunning
	agent.StartedAt = &recent
	m.mu.Unlock()

	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	if len(restarted) != 0 {
		t.Errorf("agent within boot grace should not restart, got %v", restarted)
	}
}

// TestCheckAndRestartCrashedAgents_InferenceStall: an inference agent with a
// live CLI pane (not a consent screen) goes through the stall-check path.
func TestCheckAndRestartCrashedAgents_InferenceHealthy(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()
	// Live CLI marker, not a consent screen.
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "-l", ": Claude esc to interrupt").Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "Enter").Run()
	time.Sleep(400 * time.Millisecond)

	old := time.Now().Add(-2 * time.Minute)
	m.mu.Lock()
	agent.State = StateRunning
	agent.StartedAt = &old
	m.mu.Unlock()

	// Healthy inference pane -> no crash restart; exercises the stall/consent
	// bookkeeping branches.
	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	if len(restarted) != 0 {
		t.Errorf("healthy inference agent should not restart, got %v", restarted)
	}
}

// TestCheckAndRestartCrashedAgents_ConsentStuck: an inference agent parked on a
// consent screen is not restarted but goes through the consent-stuck path.
func TestCheckAndRestartCrashedAgents_ConsentStuck(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "vllm"}}, discardLogger(), ProjectContext{})
	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	m.terminal = funcTerminal{captureVisiblePane: func(*AgentProcess) string {
		return "Bypass Permissions mode\n❯ 1. No, exit\nEnter to confirm\n"
	}}

	old := time.Now().Add(-2 * time.Minute)
	m.mu.Lock()
	agent.State = StateRunning
	agent.StartedAt = &old
	m.mu.Unlock()

	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	if len(restarted) != 0 {
		t.Errorf("consent-stuck agent should not restart, got %v", restarted)
	}
	if agent.consentSeenAt.IsZero() {
		t.Error("consent-stuck path should record when the screen was first observed")
	}
}

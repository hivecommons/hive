package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSendKick_DeliversToReadyPane covers SendKick's full success path
// (crash/consent check -> waitForInputPromptForAgent -> deliverKickLocked)
// against a manually-created, ready tmux session. The agent is NOT launched via
// launchInTmux, so there is NO concurrent pollTmuxOutputForAgent goroutine
// touching agent.KickRefused — the kick delivery runs race-free (the
// poll-vs-locked-write interleaving is a property of live launches, not
// something a coverage test should exercise concurrently).
func TestSendKick_DeliversToReadyPane(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude", ClearOnKick: true},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	session := "hive-sendkick-ready"
	agent.tmuxSession = session
	newRawTmuxSession(t, session)
	// Render a ready input prompt so the crash/consent checks pass and
	// waitForInputPromptForAgent returns on its first tick.
	paneInject(t, session, "goose is ready")

	m.mu.Lock()
	agent.State = StateRunning
	m.mu.Unlock()

	if err := m.SendKick("cxa", "do the work"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}

	m.mu.RLock()
	lastMsg := agent.LastKickMessage
	m.mu.RUnlock()
	if lastMsg != "do the work" {
		t.Errorf("LastKickMessage = %q", lastMsg)
	}
}

package agent

import (
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
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
	session := agent.tmuxSession
	m.mu.RUnlock()

	if err := testTmuxCommand("new-session", "-d", "-s", session).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", session).Run()
	// Render a ready input prompt so the crash/consent checks pass and
	// waitForInputPromptForAgent returns on its first tick.
	testTmuxCommand("send-keys", "-t", session, "-l", ": goose is ready").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	time.Sleep(500 * time.Millisecond)

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

package agent

import (
	"os/exec"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// TestDismissInferencePrompts_PressEnter covers the "Press Enter to continue"
// branch: the pane shows that text, then flips to a ready marker so the loop
// returns.
func TestDismissInferencePrompts_PressEnter(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covpressenter"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covpe": {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covpe"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	exec.Command("tmux", "send-keys", "-t", session, "-l", ": Press Enter to continue").Run()
	exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
	time.Sleep(400 * time.Millisecond)

	go func() {
		time.Sleep(1200 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", session, "-l", ": esc to interrupt").Run()
		exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
	}()

	done := make(chan struct{})
	go func() { m.dismissInferencePrompts(agent); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Error("dismissInferencePrompts should return")
	}
}

// TestDismissInferencePrompts_FixWithSettingsError covers the settings-error
// "Fix with Claude" navigation branch (double Down past Fix/Exit to Continue).
func TestDismissInferencePrompts_FixWith(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covfixwith"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covfw": {Backend: "vllm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covfw"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	// Selection menu whose selected line begins with "Fix with".
	exec.Command("tmux", "send-keys", "-t", session, "-l", ": Enter to confirm").Run()
	exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
	exec.Command("tmux", "send-keys", "-t", session, "-l", "❯ Fix with Claude").Run()
	exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
	time.Sleep(400 * time.Millisecond)

	go func() {
		time.Sleep(1500 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", session, "-l", ": esc to interrupt").Run()
		exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
	}()

	done := make(chan struct{})
	go func() { m.dismissInferencePrompts(agent); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Error("dismissInferencePrompts should return")
	}
}

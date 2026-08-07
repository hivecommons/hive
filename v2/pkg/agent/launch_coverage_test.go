package agent

import (
	"context"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// TestStart_CLIAlreadyRunning exercises launchInTmux's early-return branch: the
// tmux pane already shows a CLI marker and forceRelaunch is false, so the
// manager adopts the running CLI instead of launching a new one.
func TestStart_CLIAlreadyRunning(t *testing.T) {
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

	// Pre-create the session and render a CLI marker.
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()
	// Inject a marker; forceRelaunch defaults to false.
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "-l", ": goose is ready").Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "Enter").Run()
	time.Sleep(500 * time.Millisecond)

	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")

	ap, _ := m.GetStatus("cxa")
	if ap.State != StateRunning {
		t.Errorf("adopted CLI should be running, got %s", ap.State)
	}
}

// TestStart_InferenceCLIAlreadyRunning covers the inference variant of the
// adopt-running-CLI branch (which also re-arms dismissInferencePrompts).
func TestStart_InferenceCLIAlreadyRunning(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "litellm", Model: "x"},
	}, discardLogger(), ProjectContext{})
	m.SetInferenceCallbacks(func(string, string, string) {}, func(string) {})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "-l", ": esc to interrupt bypass permissions on Claude").Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "Enter").Run()
	time.Sleep(500 * time.Millisecond)

	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

// TestStart_CopilotAdoptsRunning covers the copilot adopt branch (which also
// spawns watchForTrustPromptForAgent).
func TestStart_CopilotAdoptsRunning(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot", Model: "auto"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		t.Skipf("cannot create tmux session: %v", err)
	}
	defer testTmuxCommand("kill-session", "-t", agent.tmuxSession).Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "-l", ": Copilot ready").Run()
	testTmuxCommand("send-keys", "-t", agent.tmuxSession, "Enter").Run()
	time.Sleep(500 * time.Millisecond)

	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

// TestDismissInferencePrompts_NavigatesNegativeSelection drives the menu
// navigation branch: a selection prompt whose selected option is negative
// ("No, exit") so the function presses Down then Enter, then the pane flips to
// a ready marker so it returns.
func TestDismissInferencePrompts_NavigatesNegative(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covdismiss2"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covdismiss2": {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covdismiss2"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	// Render a selection prompt with a negative selected option.
	testTmuxCommand("send-keys", "-t", session, "-l", ": Enter to confirm").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	testTmuxCommand("send-keys", "-t", session, "-l", "❯ 1. No, exit").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	time.Sleep(400 * time.Millisecond)

	// Run dismissal briefly; flip the pane to ready shortly after so it returns.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		testTmuxCommand("send-keys", "-t", session, "-l", ": esc to interrupt").Run()
		testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	}()

	done := make(chan struct{})
	go func() {
		m.dismissInferencePrompts(agent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("dismissInferencePrompts should return after pane becomes ready")
	}
}

// TestDismissInferencePrompts_BypassConsent covers the explicit bypass-consent
// handling branch (confirmMenuOption with Down navigation).
func TestDismissInferencePrompts_BypassConsent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covdismiss3"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covdismiss3": {Backend: "vllm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covdismiss3"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	testTmuxCommand("send-keys", "-t", session, "-l", ": Bypass Permissions mode").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	testTmuxCommand("send-keys", "-t", session, "-l", "❯ 2. Yes, I accept").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	time.Sleep(400 * time.Millisecond)

	go func() {
		time.Sleep(1500 * time.Millisecond)
		testTmuxCommand("send-keys", "-t", session, "-l", ": esc to interrupt").Run()
		testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	}()

	done := make(chan struct{})
	go func() {
		m.dismissInferencePrompts(agent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("dismissInferencePrompts should return")
	}
}

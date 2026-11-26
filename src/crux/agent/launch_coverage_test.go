package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

// seedAdoptablePane prepares the agent's tmux session showing a CLI marker so
// Start takes launchInTmux's adopt-running-CLI branch, and returns only once
// the manager itself sees that marker.
//
// The session goes through newRawTmuxSession rather than a bare new-session:
//   - it lands on the package test tmux server (-L defaultTmuxSocket), the
//     same server the manager inspects (#4628);
//   - it kills any session of the same name first. Every adopt test uses the
//     fixed "cxa" agent (session "hive-cxa"), shared with many other tests in
//     this package whose async relaunch machinery can re-create that session
//     after their own cleanup — a bare new-session then fails "duplicate
//     session" (#6618, #19963);
//   - it uses the production 500-column pane, so a long shell prompt cannot
//     wrap the marker out of the visible-pane substring match.
//
// Waiting on tmuxPaneHasCLIForAgent instead of a fixed sleep removes the
// other race: on a loaded runner the typed marker may not have rendered yet,
// so Start would take the fresh-launch branch instead of the one under test.
func seedAdoptablePane(t *testing.T, m *Manager, agent *AgentProcess, marker string) {
	t.Helper()
	newRawTmuxSession(t, agent.tmuxSession)
	if err := testTmuxCommand("send-keys", "-t", agent.tmuxSession, "-l", ": "+marker).Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if err := testTmuxCommand("send-keys", "-t", agent.tmuxSession, "Enter").Run(); err != nil {
		t.Fatalf("send-keys Enter: %v", err)
	}
	testutil.EventuallyEveryFunc(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return m.tmuxPaneHasCLIForAgent(agent)
	}, func() string {
		return fmt.Sprintf("pane never showed CLI marker %q; pane:\n%s", marker, m.captureVisiblePaneForAgent(agent))
	})
}

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

	seedAdoptablePane(t, m, agent, "goose is ready")

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
	// The adopt branch re-arms dismissInferencePrompts, which does not watch
	// ctx. cleanupAgent can kill the pane before that goroutine's first poll,
	// leaving it polling an empty pane until its timeout — 60s by default,
	// long past this test and into the next test that reuses "hive-cxa".
	// Bound it well inside cleanupAgent's 5s drain.
	m.promptDismissTimeout = 2 * time.Second

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	seedAdoptablePane(t, m, agent, "esc to interrupt bypass permissions on Claude")

	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")

	ap, _ := m.GetStatus("cxa")
	if ap.State != StateRunning {
		t.Errorf("adopted inference CLI should be running, got %s", ap.State)
	}
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

	seedAdoptablePane(t, m, agent, "Copilot ready")

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
	m, agent, script := newDismissPromptHarness(t, "covdismiss2", ": Enter to confirm\n❯ 1. No, exit", 2)
	runDismissPromptScript(t, m, agent, script, []string{"Down", "Enter"})
}

// TestDismissInferencePrompts_BypassConsent covers the explicit bypass-consent
// handling branch (confirmMenuOption with Down navigation).
func TestDismissInferencePrompts_BypassConsent(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "covdismiss3", ": Bypass Permissions mode\n❯ 1. No, exit", 2)
	script.afterDownPane = ": Bypass Permissions mode\n❯ 2. Yes, I accept"
	runDismissPromptScript(t, m, agent, script, []string{"Down", "Enter"})
}

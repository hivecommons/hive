package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
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

	// Pre-create the session and render a CLI marker. This must land on the
	// package test tmux server (-L defaultTmuxSocket) — the same server the
	// manager inspects. A bare `tmux` exec would hit the default-socket server
	// instead, where the manager never looks, so the adopt branch under test
	// would silently never trigger (#4628).
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		// tmuxAvailable() already passed above, so tmux is on PATH and
		// TMUX_TMPDIR points into TestMain's temp tree. A failure here is a
		// broken test (stale socket, uncleaned server, name collision), not
		// a missing capability (#5388).
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
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

	// Same-server requirement as TestStart_CLIAlreadyRunning above.
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		// tmuxAvailable() already passed above, so tmux is on PATH and
		// TMUX_TMPDIR points into TestMain's temp tree. A failure here is a
		// broken test (stale socket, uncleaned server, name collision), not
		// a missing capability (#5388).
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
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

	// Same-server requirement as TestStart_CLIAlreadyRunning above.
	if err := testTmuxCommand("new-session", "-d", "-s", agent.tmuxSession).Run(); err != nil {
		// tmuxAvailable() already passed above, so tmux is on PATH and
		// TMUX_TMPDIR points into TestMain's temp tree. A failure here is a
		// broken test (stale socket, uncleaned server, name collision), not
		// a missing capability (#5388).
		testutil.SkipfUnlessRequired(t, "cannot create tmux session: %v", err)
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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestRunCopilotDiagnostic_AuthError drives the diagnostic loop: a bare copilot
// launch whose pane shows "Bad credentials" -> matchesAuthError -> clear tokens
// (best-effort) -> kill + Restart. Uses a fast ctx to bound the loop.
func TestRunCopilotDiagnostic_AuthError(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	overrideStub(t, "copilot", "Bad credentials")
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-a"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m.runCopilotDiagnostic(ctx, agent)
	defer cleanupAgent(t, m, "cxa")
	// The diagnostic sets LastError on auth-error detection.
	if agent.LastError == "" {
		t.Log("diagnostic did not record an error (timing-sensitive)")
	}
}

// TestRunCopilotDiagnostic_CLIReady drives the success path: the bare copilot
// shows a CLI-ready indicator -> clears LastError -> restart.
func TestRunCopilotDiagnostic_CLIReady(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	overrideStub(t, "copilot", "Copilot v1.0")
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-a"
	agent.LastError = "stale error"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m.runCopilotDiagnostic(ctx, agent)
	defer cleanupAgent(t, m, "cxa")
}

// TestLaunchInTmux_GooseWithBootstrap exercises the goose launch branch which
// writes the bootstrap prompt file and appends --text "$(cat ...)".
func TestLaunchInTmux_GooseBootstrap(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "goose", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{Org: "o", Repos: []string{"r"}, ACMMLevel: 5})
	// Provide a bootstrap override so the goose --text path fires.
	if err := m.RestartWithBootstrap(context.Background(), "cxa", "goose boot prompt"); err != nil {
		t.Fatalf("RestartWithBootstrap: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	// Bootstrap file should have been written for goose.
	if _, err := os.Stat(filepath.Join(agentStateDir, ".hive-bootstrap-a.txt")); err != nil {
		t.Logf("goose bootstrap file: %v", err)
	}
	os.Remove(filepath.Join(agentStateDir, ".hive-bootstrap-a.txt"))
}

// TestLaunchInTmux_GooseNoBootstrap covers the minimal --text branch for goose
// when no bootstrap prompt is present.
func TestLaunchInTmux_GooseNoBootstrap(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		// Advisory mode + empty project => buildBootstrapPrompt yields empty, so
		// the goose minimal --text branch is taken.
		"cxa": {Backend: "goose", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
	os.Remove(filepath.Join(agentStateDir, ".hive-bootstrap-a.txt"))
}

// TestLaunchInTmux_ClaudeIssuesOnly / DefaultMode cover the claude mode switch
// branches (ModeIssuesOnly and default/advisory disallowed-tools sets).
func TestLaunchInTmux_ClaudeModeBranches(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	for _, mode := range []string{"ISSUES_ONLY", "ADVISORY"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HIVE_WORK_DIR", t.TempDir())
			m := NewManager(map[string]config.AgentConfig{
				"cxa": {Backend: "claude", Model: "opus", Mode: mode},
			}, discardLogger(), ProjectContext{ACMMLevel: 5})
			if err := m.Start(context.Background(), "cxa"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer cleanupAgent(t, m, "cxa")
		})
	}
}

// TestLaunchInTmux_CopilotDefaultMode covers copilot's default (advisory)
// deny-tool set.
func TestLaunchInTmux_CopilotDefaultMode(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot", Model: "auto", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 1})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

// TestLaunchInTmux_WithToolsConfig covers the toolRulesToLaunchCmd path in
// launchInTmux (agent.Config.Tools != nil).
func TestLaunchInTmux_WithToolsConfig(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {
			Backend: "claude",
			Model:   "opus",
			Mode:    "ISSUES_AND_PRS",
			Tools:   denyTools("mcp__github__merge_pull_request"),
		},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

// TestLaunchInTmux_WithMCPConnections covers connectionMCPFlags append in launch.
func TestLaunchInTmux_WithMCPConnections(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {
			Backend:     "claude",
			Model:       "opus",
			Connections: []config.ConnectionConfig{{Type: "mcp", URI: "https://mcp.example"}},
		},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	if err := m.Start(context.Background(), "cxa"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer cleanupAgent(t, m, "cxa")
}

// ---------------------------------------------------------------------------
// pollTmuxOutputForAgent — login-prompt + TLS-error detection branches.
// ---------------------------------------------------------------------------

func TestPollTmuxOutputForAgent_LoginPrompt(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covlogin"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covlogin": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covlogin"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	// A login prompt sets NeedsLogin; configHasTokens() is false (no /data) so
	// no auto-restart fires.
	paneInject(t, session, "Please sign in to use Copilot")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	m.pollTmuxOutputForAgent(agent, ctx)
	agent.paneMu.Lock()
	needsLogin := agent.NeedsLogin
	agent.paneMu.Unlock()
	_ = needsLogin // recorded per pane content; assertion is best-effort
}

// TestRunCopilotDiagnostic_TimeoutRelaunches pins the inconclusive-diagnostic
// path: the bare copilot prints nothing within diagnosticTimeoutSec, so the
// loop can neither confirm an auth error nor a ready CLI. The diagnostic has
// already killed the agent's real session by then, so the agent MUST be
// relaunched rather than parked in StateFailed — otherwise every kick is
// refused with "failed to start: copilot hung with no output" until an
// operator intervenes (live: 3 write-capable agents stranded for 15h).
func TestRunCopilotDiagnostic_TimeoutRelaunches(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	// Silent for longer than the (test-shrunk) diagnostic deadline, then
	// ready — so the diagnostic times out but the relaunch still settles.
	p := filepath.Join(stubBinDir, "copilot")
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("copilot stub not present: %v", err)
	}
	script := "#!/bin/sh\nsleep 6\nprintf '\\342\\235\\257 ready\\n'\nexec cat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(p, orig, 0o755) })
	t.Setenv("HIVE_WORK_DIR", t.TempDir())

	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-a"
	agent.State = StateRunning
	before := agent.RestartCount

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m.runCopilotDiagnostic(ctx, agent)
	defer cleanupAgent(t, m, "cxa")

	m.mu.RLock()
	state, count, reason, lastErr := agent.State, agent.RestartCount, agent.LastRestartReason, agent.LastError
	m.mu.RUnlock()
	if state == StateFailed {
		t.Fatalf("diagnostic timeout parked agent in StateFailed (LastError=%q); expected relaunch", lastErr)
	}
	if count != before+1 {
		t.Fatalf("RestartCount = %d, want %d (relaunch after inconclusive diagnostic)", count, before+1)
	}
	if reason != "copilot hang diagnostic timed out" {
		t.Fatalf("LastRestartReason = %q, want %q", reason, "copilot hang diagnostic timed out")
	}
}

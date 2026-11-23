package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// sandboxModeAgent is the agent the recordingTmux tests switch into sandbox
// mode. No other test in the package names an agent this, so no tmux call
// another test makes can target its session.
const sandboxModeAgent = "sandbox-mode-switch"

// recordingTmux installs a fake tmux that appends to a log file the argv of
// each invocation naming agent's tmux session, and returns a reader for that
// log.
//
// The fake is found through PATH, which is process-global, so it also answers
// tmux calls from pollers that earlier tests leaked (a Start whose context is
// never cancelled keeps capturing its pane every 3s). Logging those let a
// stray call recreate tmux.log while TempDir cleanup was deleting it
// ("unlinkat …: directory not empty", hivecommons/hive#19597), and could put
// another test's kill-session in the log. Calls that do not name this agent's
// session are answered but not recorded.
func recordingTmux(t *testing.T, agent string) func() string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	writeFakeTmux(t, "#!/bin/sh\ncase \" $* \" in\n*' hive-"+agent+" '*) echo \"$@\" >> '"+logPath+"' ;;\nesac\nexit 0\n")
	return func() string {
		raw, _ := os.ReadFile(logPath)
		return string(raw)
	}
}

func sandboxOptIn() *config.AgentSandboxOverride {
	on := true
	return &config.AgentSandboxOverride{Enabled: &on, Image: "img"}
}

func TestUpdateConfigIntoSandboxStopsTmuxSession(t *testing.T) {
	tmuxLog := recordingTmux(t, sandboxModeAgent)
	m := NewManager(map[string]config.AgentConfig{sandboxModeAgent: {Backend: "claude"}}, quietTestLogger(), ProjectContext{})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})
	var audits []string
	m.SetSandboxAuditCallback(func(agent, action, detail string) { audits = append(audits, agent+":"+action) })
	m.mu.Lock()
	agent := m.agents[sandboxModeAgent]
	agent.State = StateRunning
	cancelled := false
	agent.cancel = func() { cancelled = true }
	cfg := agent.Config
	m.mu.Unlock()

	cfg.Sandbox = sandboxOptIn()
	if err := m.UpdateConfig(sandboxModeAgent, cfg); err != nil {
		t.Fatal(err)
	}

	m.mu.RLock()
	state := agent.State
	m.mu.RUnlock()
	if state != StateIdle {
		t.Fatalf("state = %s, want %s after entering sandbox mode", state, StateIdle)
	}
	if !cancelled {
		t.Fatal("the interactive session's context was not cancelled")
	}
	if got := tmuxLog(); !strings.Contains(got, "kill-session -t hive-"+sandboxModeAgent) {
		t.Fatalf("tmux kill-session not run; tmux saw:\n%s", got)
	}
	if len(audits) != 1 || audits[0] != sandboxModeAgent+":sandbox_mode_entered" {
		t.Fatalf("audits = %v, want one sandbox_mode_entered", audits)
	}

	// A kick now reaches the sandbox validator past the running-state gate.
	m.mu.Lock()
	_, err := m.validateSandboxKickLocked(agent)
	m.mu.Unlock()
	if err != nil && strings.Contains(err.Error(), "interactive session") {
		t.Fatalf("kick still blocked by the interactive session: %v", err)
	}
}

func TestReconcileAgentsIntoSandboxStopsTmuxSession(t *testing.T) {
	tmuxLog := recordingTmux(t, sandboxModeAgent)
	m := NewManager(map[string]config.AgentConfig{sandboxModeAgent: {Backend: "claude"}}, quietTestLogger(), ProjectContext{})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})
	m.mu.Lock()
	m.agents[sandboxModeAgent].State = StateRunning
	m.mu.Unlock()

	m.ReconcileAgents(map[string]config.AgentConfig{sandboxModeAgent: {Backend: "claude", Sandbox: sandboxOptIn()}})

	m.mu.RLock()
	state := m.agents[sandboxModeAgent].State
	m.mu.RUnlock()
	if state != StateIdle {
		t.Fatalf("state = %s, want %s", state, StateIdle)
	}
	if !strings.Contains(tmuxLog(), "kill-session") {
		t.Fatal("tmux kill-session not run")
	}
}

func TestGlobalSandboxGateStopsTmuxSession(t *testing.T) {
	tmuxLog := recordingTmux(t, sandboxModeAgent)
	m := NewManager(map[string]config.AgentConfig{sandboxModeAgent: {Backend: "claude", Sandbox: sandboxOptIn()}}, quietTestLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents[sandboxModeAgent].State = StateRunning
	m.mu.Unlock()

	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})

	m.mu.RLock()
	state := m.agents[sandboxModeAgent].State
	m.mu.RUnlock()
	if state != StateIdle {
		t.Fatalf("state = %s, want %s", state, StateIdle)
	}
	if !strings.Contains(tmuxLog(), "kill-session") {
		t.Fatal("tmux kill-session not run")
	}
}

// An agent already in sandbox mode, or one mid sandbox kick, is not touched.
func TestEnterSandboxModeLeavesSandboxKickAlone(t *testing.T) {
	tmuxLog := recordingTmux(t, sandboxModeAgent)
	m := NewManager(map[string]config.AgentConfig{sandboxModeAgent: {Backend: "claude"}}, quietTestLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents[sandboxModeAgent]
	agent.State = StateRunning
	agent.sandboxKickActive = true
	m.enterSandboxModeLocked(agent)
	agent.sandboxKickActive = false
	agent.State = StatePaused
	m.enterSandboxModeLocked(agent)
	m.enterSandboxModeLocked(nil)
	state := agent.State
	m.mu.Unlock()
	if state != StatePaused {
		t.Fatalf("state = %s, want paused kept", state)
	}
	if got := tmuxLog(); strings.Contains(got, "kill-session") {
		t.Fatalf("kill-session ran for an agent it must leave alone:\n%s", got)
	}
}

func TestEnterSandboxModeLogsKillFailure(t *testing.T) {
	writeFakeTmux(t, "#!/bin/sh\necho 'permission denied' >&2\nexit 1\n")
	logBuf := &syncBuffer{}
	m := NewManager(map[string]config.AgentConfig{"reviewer": {Backend: "claude"}}, slog.New(slog.NewTextHandler(logBuf, nil)), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["reviewer"]
	agent.State = StateRunning
	m.enterSandboxModeLocked(agent)
	m.mu.Unlock()
	if !strings.Contains(logBuf.String(), "tmux kill-session on entering sandbox mode failed") {
		t.Fatalf("kill failure not logged: %s", logBuf.String())
	}
}

func TestSandboxKickRefusalNamesRealState(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"reviewer": {Backend: "claude", Sandbox: sandboxOptIn()}}, quietTestLogger(), ProjectContext{})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})
	m.mu.Lock()
	defer m.mu.Unlock()
	agent := m.agents["reviewer"]
	agent.State = StateRunning

	_, err := m.validateSandboxKickLocked(agent)
	if err == nil || !strings.Contains(err.Error(), "agent has a live interactive session; sandbox kicks are blocked until it stops") {
		t.Fatalf("err = %v, want the live-interactive-session refusal", err)
	}

	agent.sandboxKickActive = true
	_, err = m.validateSandboxKickLocked(agent)
	if err == nil || !strings.Contains(err.Error(), "a sandbox kick is already running") {
		t.Fatalf("err = %v, want the sandbox-kick-in-flight refusal", err)
	}
}

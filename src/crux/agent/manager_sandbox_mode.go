package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// sandboxModeArchiveReason labels the kick-log archive written when an agent's
// interactive session is retired because it switched to sandbox execution.
const sandboxModeArchiveReason = "sandbox-mode"

// sandboxKickBlockedError is the refusal for a sandbox kick to an agent in
// StateRunning. It names what is actually holding the agent: a sandbox kick
// still in flight, or an interactive tmux session left over from before the
// agent was switched to sandbox mode (hivecommons/hive#18525). The second
// used to read "sandbox execution already running", which sent operators
// looking for a sandbox run that did not exist.
func sandboxKickBlockedError(agent *AgentProcess) error {
	if agent.sandboxKickActive {
		return fmt.Errorf("agent %s cannot be kicked: a sandbox kick is already running", agent.Name)
	}
	return fmt.Errorf("agent %s cannot be kicked: agent has a live interactive session; sandbox kicks are blocked until it stops", agent.Name)
}

// enterSandboxModeLocked retires the interactive tmux session of an agent
// whose config just switched it to sandbox execution. Sandboxed agents never
// run a CLI in tmux, and a session left running keeps the agent in
// StateRunning, which refuses every sandbox kick (hivecommons/hive#18525).
// The session's scrollback is archived first, the agent's CLI is reaped and
// the session killed, and a running agent drops to StateIdle — the state a
// sandboxed agent waits in between kicks. Paused and stopped agents keep
// their state. Callers must hold m.mu.
func (m *Manager) enterSandboxModeLocked(agent *AgentProcess) {
	if agent == nil || agent.sandboxKickActive || agent.State != StateRunning {
		return
	}
	m.archiveKickLogLocked(agent, sandboxModeArchiveReason)
	if agent.cancel != nil {
		agent.cancel()
		agent.cancel = nil
	}
	reaped := m.reapAgentCLI(agent)
	if output, err := m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).CombinedOutput(); err != nil && !tmuxKillSessionNothingToKill(string(output)) {
		m.logger.Warn("tmux kill-session on entering sandbox mode failed",
			"name", agent.Name,
			"session", agent.tmuxSession,
			"error", err,
			"stderr", strings.TrimSpace(string(output)))
	}
	now := time.Now()
	agent.State = StateIdle
	agent.StartedAt = &now
	agent.HasLaunched = true
	agent.LaunchedMode = m.agentMode(agent)
	m.logger.Info("audit: agent switched to sandbox mode; interactive session stopped",
		"name", agent.Name, "session", agent.tmuxSession, "reaped_cli", reaped)
	m.auditSandbox(agent.Name, "sandbox_mode_entered", "interactive tmux session "+agent.tmuxSession+" stopped")
}

// UpdateConfig updates the stored config for a running agent process so that
// status builders (which read from AgentProcess.Config) reflect changes made
// via the config dialog (which writes to the global Config.Agents map).
func (m *Manager) UpdateConfig(name string, cfg config.AgentConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	wasSandboxed := m.agentSandboxEnabledLocked(agent)
	agent.clearAgentSpecPromptIfSpecRemoved(cfg)
	agent.Config = cfg
	agent.baseConfig = cfg
	if !wasSandboxed && m.agentSandboxEnabledLocked(agent) {
		m.enterSandboxModeLocked(agent)
	}
	return nil
}

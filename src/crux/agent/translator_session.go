package agent

import (
	"context"
	"strings"
	"time"
)

// inferenceTranslatorBaseURL is where an inference-routed Claude CLI sends
// its Anthropic API calls: hive's translator, which forwards them on the
// agent's inference route. agentEnvPairs exports it as ANTHROPIC_BASE_URL
// for routable backends only.
const inferenceTranslatorBaseURL = "http://127.0.0.1:18444"

// translatorSessionProbeTimeout bounds the show-environment probe, which runs
// under m.mu: a wedged tmux server must not hold the manager lock.
const translatorSessionProbeTimeout = 5 * time.Second

// sessionEnvRoutesToTranslator reports whether the agent's tmux session
// environment points ANTHROPIC_BASE_URL at the inference translator. The pane
// shell is forked from that environment when the session is created (see
// ensureTmuxSession), so a true answer means every CLI typed into the pane
// inherits the translator URL and the sk-hive-<agent> key unless the launch
// line overrides them. A probe failure answers false: the launch then behaves
// exactly as before.
func (m *Manager) sessionEnvRoutesToTranslator(agent *AgentProcess) bool {
	ctx, cancel := context.WithTimeout(context.Background(), translatorSessionProbeTimeout)
	defer cancel()
	out, err := m.tmuxCmdContext(ctx, agent, "show-environment", "-t", agent.tmuxSession, "ANTHROPIC_BASE_URL").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "ANTHROPIC_BASE_URL="+inferenceTranslatorBaseURL
}

// retireTranslatorSessionLocked recreates the agent's tmux session when it was
// built for an inference-routed launch and the agent is about to launch on a
// backend that is not routed.
//
// A non-routed launch line carries no ANTHROPIC_BASE_URL, so the CLI inherits
// the pane shell's: the translator. The launch has just cleared the agent's
// route, so every request then fails with "inference no route"
// (reason=cleared) until the session is rebuilt (hivecommons/hive#19811).
// Restart rebuilds the session, but Stop and Pause leave it (and its shell)
// in place, and a backend switch on a stopped or paused agent skips the
// restart (the dashboard switch, provider rotation of a stopped agent), so
// the next Start or Resume types the CLI into the old shell. Killing the
// session and running ensureTmuxSession again gives the pane a shell forked
// from the current backend's environment. Caller holds m.mu.
func (m *Manager) retireTranslatorSessionLocked(agent *AgentProcess) error {
	if !m.sessionEnvRoutesToTranslator(agent) {
		return nil
	}
	if output, err := m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).CombinedOutput(); err != nil && !tmuxKillSessionNothingToKill(string(output)) {
		m.logger.Warn("tmux kill-session of a translator-routed session failed; the CLI may inherit the translator URL",
			"name", agent.Name,
			"session", agent.tmuxSession,
			"error", err,
			"stderr", strings.TrimSpace(string(output)))
	}
	m.logger.Info("recreating tmux session built for inference routing before a non-routed launch",
		"name", agent.Name, "session", agent.tmuxSession, "backend", agent.effectiveBackend())
	return m.ensureTmuxSession(agent)
}

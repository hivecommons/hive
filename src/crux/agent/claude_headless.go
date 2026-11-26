package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// claudeSessionFileVar names the pane-shell variable holding the file that
// records the Claude session id between headless_turns kicks (see
// claude_turn.go). Like agyConversationFileVar, the launch shim creates it
// with mktemp, so every (re)launch starts a fresh session, matching the
// interactive CLI it replaces, and no other agent can predict the path.
const claudeSessionFileVar = "HIVE_CLAUDE_SESSION_FILE"

// agentUsesClaudeHeadless reports whether an agent on the given effective
// backend runs one `claude -p` per kick instead of a resident interactive
// Claude Code session (headless_turns, #19902). Only translator-routed
// backends — inference backends and configured gateway names — qualify: they
// are the ones launched as claude against Hive's translator. An operator
// launch_cmd replaces the shim, as it does for agy. Lock-free, so the launch
// and kick paths can call it with m.mu held.
//
// The CLI still talks to Hive's translator, so key custody, metering and
// /v1/models discovery are unchanged; only how the CLI is started changes.
// Anthropic bills `claude -p` run with a Console API key as Agent SDK usage,
// which subscriber API credits cover, whereas they do not cover interactive
// Claude Code. AgentProcess.claudeHeadlessTurnCmd holds the per-kick claude
// line; it is rebuilt on every launch pass, the surviving-pane path included,
// so a hub restart keeps it.
func (m *Manager) agentUsesClaudeHeadless(backend string, agent *AgentProcess) bool {
	if agent == nil || !agent.Config.HeadlessTurnsEnabled() || !m.routableBackend(backend) {
		return false
	}
	cmd, _ := effectiveLaunchCmd(agent, backend)
	return cmd == ""
}

// paneIsClaudeHeadless reports whether the agent's CURRENT pane was launched
// as a headless_turns shell. Kick-time routing (kick delivery, the inference
// watchdog, SendResumeKick) must follow the pane, not the live config: the
// pane's shape is fixed at launch, so toggling headless_turns has no effect
// until the next relaunch, and a kick never types C-c, /clear or the raw
// prompt into the bash shim. claudeHeadlessTurnCmd is set or cleared on every
// launch pass, the surviving-pane path included.
func paneIsClaudeHeadless(agent *AgentProcess) bool {
	return agent != nil && strings.TrimSpace(agent.claudeHeadlessTurnCmd) != ""
}

// paneShapeMatchesHeadlessLocked reports whether the agent's live pane has the
// shape a launch with the given headless decision would build: the
// headless_turns shim shows its ready (or running) marker, an interactive CLI
// shows neither. Used to refuse reusing a surviving pane after headless_turns
// was toggled while the hub was down. Caller holds m.mu.
func (m *Manager) paneShapeMatchesHeadlessLocked(agent *AgentProcess, headless bool) bool {
	pane := m.captureVisiblePaneForAgent(agent)
	paneHeadless := strings.Contains(pane, claudeHeadlessReadyMarker) ||
		strings.Contains(pane, claudeHeadlessRunningMarker)
	return paneHeadless == headless
}

// claudeHeadlessLaunchCmd is the line typed into the pane at launch: no CLI
// runs until the first kick. It creates the per-launch session file and sets
// a PS1 carrying the ready marker for the launch, reuse and kick gates.
func claudeHeadlessLaunchCmd() string {
	// A relaunch into a surviving pane shell re-runs this line: drop the
	// previous launch's file first so it neither leaks nor gets resumed.
	return fmt.Sprintf(`[ -n "${%[1]s:-}" ] && rm -f "$%[1]s"; export %[1]s="$(mktemp "${TMPDIR:-/tmp}/hive-claude-session.XXXXXX")"; trap 'rm -f "$%[1]s"' EXIT; export PS1=%[2]s; echo %[3]s`,
		claudeSessionFileVar,
		shellQuote(claudeHeadlessReadyMarker+"> "), shellQuote(claudeHeadlessReadyMarker))
}

// claudeHeadlessTurnShellCommand is the line a kick types into a
// headless_turns pane: one `hive claude-turn` run, then the ready marker.
// claudeCmd is the agent's claude launch line (binary, model, --bare
// --settings, deny rules), already shell-quoted for typing into a pane; the
// runner adds the print-mode flags and --resume. The prompt stays in a file
// and reaches claude on stdin.
func claudeHeadlessTurnShellCommand(hiveBinary, claudeCmd, promptFile string, newSession bool) string {
	args := []string{shellQuote(hiveBinary), ClaudeTurnSubcommand,
		"--prompt-file", shellQuote(promptFile),
		"--session-file", fmt.Sprintf(`"${%s:-}"`, claudeSessionFileVar)}
	if newSession {
		args = append(args, "--new-session")
	}
	args = append(args, "--", strings.TrimSpace(claudeCmd))
	return fmt.Sprintf("printf '\\n%s\\n'; %s; echo %s",
		claudeHeadlessRunningMarker, strings.Join(args, " "), claudeHeadlessReadyMarker)
}

// errClaudeHeadlessNotLaunched is returned by a kick that reaches a
// headless_turns agent whose claude command was never built, e.g. a kick
// racing the first launch. The kick is refused rather than typed blind.
var errClaudeHeadlessNotLaunched = errors.New("headless claude command not built yet")

// deliverClaudeHeadlessKickLocked runs one headless Claude turn for a kick.
// skipClear is the resume-kick flag: a resumed kick continues the recorded
// session even when ClearOnKick is on. Caller holds m.mu.
func (m *Manager) deliverClaudeHeadlessKickLocked(agent *AgentProcess, message, trigger string, skipClear bool) error {
	claudeCmd := agent.claudeHeadlessTurnCmd
	if strings.TrimSpace(claudeCmd) == "" {
		return m.kickDeliveryFailedLocked(agent, "build headless claude turn", errClaudeHeadlessNotLaunched)
	}
	if err := m.tmuxSendKeysForAgent(agent, "C-u"); err != nil {
		return m.kickDeliveryFailedLocked(agent, "clear input", err)
	}
	time.Sleep(staleCheckDelay)

	// The CLI still talks to an inference route, so the action-forcing
	// suffix weak OSS models need applies here as on the interactive path.
	message = kickMessageWithSuffixes(message, true, resolveExplainMode(agent.Config, m.explainModeDefault()))
	promptFile := filepath.Join(agentStateDir, fmt.Sprintf(".hive-claude-prompt-%s.txt", agent.Name))
	if err := writeAgentStateFile(promptFile, []byte(message)); err != nil {
		m.logger.Warn("failed to write headless claude prompt", "name", agent.Name, "error", err)
		return m.kickDeliveryFailedLocked(agent, "write headless prompt", err)
	}

	// ClearOnKick maps to a new session: there is no TUI to /clear.
	newSession := agent.Config.ClearOnKick && !skipClear
	cmd := claudeHeadlessTurnShellCommand(agyTurnRunnerBinary(), claudeCmd, promptFile, newSession)
	if err := m.tmuxSendLiteralForAgent(agent, cmd); err != nil {
		return m.kickDeliveryFailedLocked(agent, "send text", err)
	}
	time.Sleep(textToEnterDelay)
	if err := m.tmuxSendEntersForAgent(agent); err != nil {
		return m.kickDeliveryFailedLocked(agent, "submit prompt", err)
	}

	m.recordDeliveredKickLocked(agent, message, trigger)
	return nil
}

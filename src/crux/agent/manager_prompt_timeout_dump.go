package agent

import "time"

// promptTimeoutDumpInterval is the minimum spacing between two WARN pane
// dumps for one agent's prompt timeouts (#19376).
var promptTimeoutDumpInterval = 10 * time.Minute

// claimPromptTimeoutDump reports whether this prompt timeout may log the WARN
// pane dump for agent: true at most once per promptTimeoutDumpInterval, so
// concurrent waits that time out together dump the pane once. A refused claim
// is counted and reported on the next dump that goes through.
func (m *Manager) claimPromptTimeoutDump(agent *AgentProcess, now time.Time) bool {
	for {
		last := agent.promptTimeoutDumpAt.Load()
		if last != 0 && now.Sub(time.Unix(0, last)) < promptTimeoutDumpInterval {
			agent.promptTimeoutDumpsSuppressed.Add(1)
			return false
		}
		if agent.promptTimeoutDumpAt.CompareAndSwap(last, now.UnixNano()) {
			return true
		}
	}
}

// promptTimeoutAlreadyDumped logs a prompt timeout whose pane dump was refused
// by claimPromptTimeoutDump at INFO and returns the not-ready reason the
// prompt wait reports for it.
func (m *Manager) promptTimeoutAlreadyDumped(agent *AgentProcess, dialogTitle string, hasDialog bool) string {
	m.logger.Info("prompt timeout; pane already dumped recently",
		"agent", agent.Name,
		"session", agent.tmuxSession,
		"permission_dialog", dialogTitle,
		"dump_interval", promptTimeoutDumpInterval)
	if hasDialog {
		return claudePermissionDialogNotReadyReason(dialogTitle)
	}
	return kickPromptNotReadyReason
}

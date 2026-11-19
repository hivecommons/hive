package agent

// Operator-token mode for Claude (#17760).
//
// With HIVE_CLAUDE_OAUTH_TOKEN set, every claude-backend agent runs on the
// operator's long-lived `claude setup-token` token through
// CLAUDE_CODE_OAUTH_TOKEN (manager_env.go). That token is never refreshed, so
// the machinery built around the shared ~/.claude/.credentials.json — the
// per-agent credential re-bridge that publishes a refreshed token back into
// the shared login, and the durable-credential watchdog that alerts when that
// file is missing or spent — has nothing to act on: the file is not what the
// agents authenticate with. Acting anyway would move credential files around
// for no reason; alerting would send an operator to redo a dashboard sign-in
// that the fleet does not use. Both are therefore suspended for Claude while
// the token is set and behave exactly as before when it is unset. The token's
// own health (a 401 from the API) is surfaced separately (#17759).

// claudeOperatorTokenMode reports whether Claude agents run on the operator's
// long-lived token rather than the shared credentials file. The token is read
// once at construction (claudeOperatorToken), so the mode is fixed for the
// life of the process.
func (m *Manager) claudeOperatorTokenMode() bool {
	return m.claudeOperatorToken != ""
}

// logClaudeOperatorTokenModeOnce says, once per process, that the Claude
// credential re-bridge and watchdog alerts are suspended, so an operator
// reading the log knows why neither will ever fire. Never logs the token.
func (m *Manager) logClaudeOperatorTokenModeOnce() {
	if !m.claudeOperatorTokenMode() {
		return
	}
	m.claudeOperatorModeLogOnce.Do(func() {
		m.logger.Info("claude auth: operator-supplied long-lived token is set; Claude agents run on it, so the shared credentials file is not in use — credential re-bridge and durable-credential watchdog alerts are suspended for the claude backend",
			"source", "HIVE_CLAUDE_OAUTH_TOKEN",
			"suspended", "claude credential re-bridge, claude durable-credential watchdog")
	})
}

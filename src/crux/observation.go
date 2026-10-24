package crux

import "github.com/hivecommons/hive/crux/agent"

// Copilot token sources, as reported by Runtime.CopilotTokenSource. They are
// operator-facing phrases naming which credential GitHub saw; consumers name
// them here so they do not import crux/agent.
const (
	CopilotTokenSourceEnv            = agent.CopilotTokenSourceEnv
	CopilotTokenSourceDashboardLogin = agent.CopilotTokenSourceDashboardLogin
	CopilotTokenSourceDurableFile    = agent.CopilotTokenSourceDurableFile
	CopilotTokenSourceCLIConfig      = agent.CopilotTokenSourceCLIConfig
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token. Dashboard: Copilot auth status and login.
const CopilotUserTokenPath = agent.CopilotUserTokenPath

// GitHubTokenLogin resolves the GitHub login that owns token via GET /user, or
// "" on any failure. Dashboard: the model-discovery notice naming the account
// behind a rejected Copilot credential.
func GitHubTokenLogin(token string) string {
	return agent.GitHubTokenLogin(token)
}

// AuthPaneState is what ClassifyAuthPane reads off an agent pane: a login
// prompt (and its URL) or a backend authentication error.
type AuthPaneState = agent.AuthPaneState

// ClassifyAuthPane classifies captured pane lines for login prompts and
// backend auth errors. Loginscan: the pane login detector.
func ClassifyAuthPane(lines []string) AuthPaneState {
	return agent.ClassifyAuthPane(lines)
}

// PaneShowsBlockingPrompt reports whether the pane sits on a known
// startup-blocking modal (folder trust, update prompt, …) for backend, which
// is not a login problem. Loginscan: the login detector stands down on it.
func PaneShowsBlockingPrompt(backend, pane string) bool {
	return agent.PaneShowsBlockingPrompt(backend, pane)
}

// SanitizePaneText removes terminal/TUI chrome from pane-derived text before
// it is displayed, keeping at most maxLines lines. Dashboard: live and detail
// activity summaries.
func SanitizePaneText(text string, maxLines int) string {
	return agent.SanitizePaneText(text, maxLines)
}

// RedactPaneTokens masks GitHub tokens, sk- API keys and device-login lines in
// pane-derived text. Dashboard: activity summaries.
func RedactPaneTokens(s string) string {
	return agent.RedactPaneTokens(s)
}

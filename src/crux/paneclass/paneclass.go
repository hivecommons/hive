// Package paneclass is the runtime's pane classification surface for
// consumers: the readers that turn a captured agent pane into display text or
// a login/auth verdict. The classifiers are implemented in crux/agent, next to
// the pane poller that uses the same tables; this leaf re-exports them so the
// crux root package does not have to import crux/agent (#13962, #13963) and
// consumers still do not name crux/agent.
package paneclass

import "github.com/hivecommons/hive/crux/agent"

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
// activity summaries; discord: agent status summaries.
func SanitizePaneText(text string, maxLines int) string {
	return agent.SanitizePaneText(text, maxLines)
}

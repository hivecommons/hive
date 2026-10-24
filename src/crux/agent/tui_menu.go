package agent

import (
	"fmt"
	"strings"
	"time"
)

// tuiMenuFooters lists, per backend, the key-hint footers a CLI renders while
// a menu or modal owns the keyboard instead of the input prompt (#13765).
//
// Codex's menus (/hooks, /model, /approvals, ...) keep a "›" selection caret,
// which is also codexInputPromptMarker, so paneShowsInputPrompt accepted the
// pane as ready and every kick was typed into the menu and silently lost.
// Observed live on Codex 0.163, the /hooks screen:
//
//	PostToolUse hooks
//	Turn hooks on or off. Your changes are saved automatically.
//	› [x]  1  Unnamed hook
//	  Source    Plugin - onboard@onboard
//	  space/enter toggle · esc back
//
// Backends without an entry are unaffected: no footer ever matches, so their
// kick readiness behaves exactly as before.
var tuiMenuFooters = map[string][]string{
	codexBackend: {"esc back", "esc close", "enter details"},
}

// tuiMenuFooterScanLines bounds the footer search to the bottom of the visible
// pane, so a footer string quoted in agent output higher up never matches.
const tuiMenuFooterScanLines = 3

// tuiMenuEscapeAttempts bounds how many Escapes one readiness wait sends to
// back out of a menu before declaring the prompt not ready. Nested Codex
// menus need one Escape per level.
const tuiMenuEscapeAttempts = 3

// tuiMenuEscapeDelay lets the TUI repaint after an Escape before the pane is
// captured again.
const tuiMenuEscapeDelay = 500 * time.Millisecond

// kickPromptNotReadyReason is the generic undeliverable reason used when the
// input prompt never appeared.
const kickPromptNotReadyReason = "CLI did not reach input prompt"

// paneTUIMenuFooter returns the known menu/modal footer marker shown at the
// bottom of pane for backend, or "" when none is showing.
func paneTUIMenuFooter(backend, pane string) string {
	markers := tuiMenuFooters[backend]
	if len(markers) == 0 || pane == "" {
		return ""
	}
	lines := strings.Split(pane, "\n")
	scanned := 0
	for i := len(lines) - 1; i >= 0 && scanned < tuiMenuFooterScanLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		scanned++
		for _, marker := range markers {
			if strings.Contains(line, marker) {
				return marker
			}
		}
	}
	return ""
}

// kickPromptNotReadyErr is the error a kick returns when its prompt wait gave
// up. The generic reason keeps the historical message verbatim.
func kickPromptNotReadyErr(name, reason string) error {
	if reason == "" || reason == kickPromptNotReadyReason {
		return fmt.Errorf("agent %s CLI did not reach input prompt", name)
	}
	return fmt.Errorf("agent %s CLI did not reach input prompt: %s", name, reason)
}

// tuiMenuNotReadyReason is the undeliverable reason for a kick whose pane is
// stuck in a menu, carried into logs and the dashboard busy reason.
func tuiMenuNotReadyReason(footer string) string {
	return fmt.Sprintf("prompt-not-ready: menu footer %q", footer)
}

// dismissTUIMenuForKick sends one Escape to back out of a menu/modal before a
// kick is typed, then waits for the TUI to repaint. Caller must NOT hold m.mu.
func (m *Manager) dismissTUIMenuForKick(agent *AgentProcess, footer string, attempt int) {
	m.logger.Warn("kick target pane is in a TUI menu; sending Escape before typing",
		"agent", agent.Name, "menu_footer", footer, "attempt", attempt)
	m.tmuxSendKeysForAgent(agent, "Escape")
	m.sleepDuringPromptDismiss(tuiMenuEscapeDelay)
}

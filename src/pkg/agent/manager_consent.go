// Interactive consent/settings prompt dismissal: the inference prompt
// dismissal loop, menu confirmation, and stuck-consent recovery.
package agent

import (
	"strings"
	"time"
)

const (
	// consentConfirmFooter appears at the bottom of Claude Code interactive
	// selection screens (consent dialogs, settings-error menus).
	consentConfirmFooter = "Enter to confirm"
	// bypassConsentTitle is the heading of the --dangerously-skip-permissions
	// consent screen. Its default selection is "No, exit" — confirming it
	// terminates the CLI and leaves a bare bash pane.
	bypassConsentTitle = "Bypass Permissions mode"
	// bypassConsentDefaultOption is the default (negative) option on the
	// bypass-permissions consent screen.
	bypassConsentDefaultOption = "No, exit"
	// bypassConsentAcceptOption is the affirmative option on the
	// bypass-permissions consent screen. Its position varies between CLI
	// versions, so acceptance navigates by matching the selected-line text.
	bypassConsentAcceptOption = "Yes, I accept"
	// apiKeyPromptTitle is the heading of the custom-API-key approval prompt,
	// shown when ANTHROPIC_API_KEY is not in customApiKeyResponses.approved.
	// Its default selection is "No (recommended)" with the affirmative option
	// above it.
	apiKeyPromptTitle = "Detected a custom API key"
	// apiKeyPromptAcceptOption is the affirmative option on the
	// custom-API-key approval prompt.
	apiKeyPromptAcceptOption = "Yes"
	// cliWorkingMarker is shown while Claude Code is actively processing a
	// request; a pane in this state is never a consent screen.
	cliWorkingMarker = "esc to interrupt"
)

// paneShowsConsentScreen reports whether the pane is showing an interactive
// consent/selection screen rather than a ready CLI input prompt. Such screens
// contain a "❯"-selected menu option (e.g. "❯ 1. No, exit"), so they satisfy
// marker-based CLI presence checks ("❯" is also a cliPaneMarkers entry) — a
// kick typed into one is consumed by the menu, or by bash once the default
// "No, exit" selection terminates the CLI. Callers should pass the visible
// pane only (no scrollback): dismissed consent screens linger in history.
func paneShowsConsentScreen(pane string) bool {
	if pane == "" || strings.Contains(pane, cliWorkingMarker) {
		return false
	}
	// A known startup-blocking menu is not a ready prompt either. The generic
	// test below needs the "Enter to confirm" footer AND a "❯"-marked line;
	// codex renders neither (its footer is "Press enter to continue" and its
	// marker is "›" U+203A), so its update menu read as READY. Everything that
	// gates on readiness — the startup kick, caveman activation — then typed
	// into the menu, and the Enter confirmed its pre-selected option:
	// "1. Update now", which runs `npm install -g` as the agent UID, fails, and
	// kills the CLI. Blocking on these lets the prompt watcher answer them.
	if paneHasBlockingPrompt(pane) {
		return true
	}
	if strings.Contains(pane, bypassConsentTitle) && strings.Contains(pane, bypassConsentDefaultOption) {
		return true
	}
	if !strings.Contains(pane, consentConfirmFooter) {
		return false
	}
	for _, line := range strings.Split(pane, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			return true
		}
	}
	return false
}

// dismissInferencePrompts polls the tmux pane for Claude Code interactive
// prompts and auto-dismisses them. The "Bypass Permissions mode" consent
// screen and the custom-API-key approval prompt are handled first and
// explicitly (see confirmMenuOption): their default selections are negative
// ("No, exit" / "No (recommended)"), so confirming blind terminates the CLI
// or declines the seeded key.
// Other prompts are handled dynamically regardless of prompt text changes
// between Claude Code versions by:
//  1. Detecting "Enter to confirm" (universal prompt footer)
//  2. Finding the selected option (line with "❯" marker)
//  3. If selected option looks negative (contains "No" or "exit"), navigate
//     away from it before confirming
//  4. For "Press Enter to continue" screens, just press Enter
//
// The pane is polled fast for the first 10s — the consent screen appears
// within ~5-8s of launch and every second it lingers is a window for a kick
// to be swallowed by the menu — then at a relaxed interval.
//
// Stops when the main Claude Code input prompt appears ("esc to interrupt").
func (m *Manager) dismissInferencePrompts(agent *AgentProcess) {
	const (
		// promptFastPollWindow covers the launch window in which the consent
		// screen normally appears (~5-8s after CLI start).
		promptFastPollWindow   = 10 * time.Second
		promptFastPollInterval = 250 * time.Millisecond
		promptPollInterval     = 1 * time.Second
		promptDismissTimeout   = 60 * time.Second
		postKeystrokeDelay     = 500 * time.Millisecond
	)

	start := time.Now()
	timeout := promptDismissTimeout
	if m.promptDismissTimeout > 0 {
		timeout = m.promptDismissTimeout
	}
	deadline := start.Add(timeout)
	lastPane := ""

	for time.Now().Before(deadline) {
		interval := promptPollInterval
		if time.Since(start) < promptFastPollWindow {
			interval = promptFastPollInterval
		}
		m.sleepDuringPromptDismiss(interval)

		pane := m.captureVisiblePaneForAgent(agent)
		if pane == "" {
			continue
		}

		// Bypass-permissions consent screen: handle first and explicitly,
		// even if the pane is unchanged since the last poll (a mistimed
		// keystroke must be retried, not skipped). The affirmative option
		// sits below the default "No, exit".
		if strings.Contains(pane, bypassConsentTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("accepting bypass-permissions consent", "agent", agent.Name)
			m.confirmMenuOption(agent, bypassConsentTitle, bypassConsentAcceptOption, "Down")
			lastPane = "" // re-capture fresh on the next pass
			continue
		}

		// Custom-API-key approval prompt: the affirmative "Yes" sits ABOVE
		// the default "No (recommended)" selection, so the generic
		// Down-then-Enter fallback below would decline it.
		if strings.Contains(pane, apiKeyPromptTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("approving seeded inference API key", "agent", agent.Name)
			m.confirmMenuOption(agent, apiKeyPromptTitle, apiKeyPromptAcceptOption, "Up")
			lastPane = ""
			continue
		}

		if pane == lastPane {
			continue
		}
		lastPane = pane

		// Main prompt visible — agent is ready
		if strings.Contains(pane, "bypass permissions on") || strings.Contains(pane, "esc to interrupt") {
			m.logger.Info("inference agent ready", "agent", agent.Name)
			return
		}

		// "Press Enter to continue" screens
		if strings.Contains(pane, "Press Enter to continue") {
			m.logger.Info("inference prompt: press enter", "agent", agent.Name)
			m.tmuxSendKeysForAgent(agent, "Enter")
			continue
		}

		// Selection prompts have "Enter to confirm" footer
		if !strings.Contains(pane, "Enter to confirm") {
			continue
		}

		// Find the currently selected option (marked with ❯)
		selected := selectedMenuOption(pane)

		m.logger.Info("inference prompt detected",
			"agent", agent.Name,
			"selected", selected,
		)

		// If current selection looks negative, navigate away from it
		selectedLower := strings.ToLower(selected)
		if strings.Contains(selectedLower, "no,") || strings.Contains(selectedLower, "no ") ||
			strings.Contains(selectedLower, "exit") {
			// Try moving down first (most prompts put the positive option below)
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		} else if strings.Contains(selectedLower, "fix with") {
			// Settings error: skip past "Fix with Claude" and "Exit" to "Continue without"
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		}

		m.tmuxSendKeysForAgent(agent, "Enter")
	}

	m.logger.Warn("inference prompt dismissal timed out", "agent", agent.Name)
}

func (m *Manager) sleepDuringPromptDismiss(d time.Duration) {
	m.terminalSession().SleepDuringPromptDismiss(d)
}

// selectedMenuOption returns the trimmed text of the "❯"-selected line of an
// interactive CLI menu, or "" if no line is selected.
func selectedMenuOption(pane string) string {
	for _, line := range strings.Split(pane, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			return trimmed
		}
	}
	return ""
}

// confirmMenuOption drives an interactive CLI menu identified by title to the
// option whose text contains want, then confirms it with Enter. Navigation
// matches the "❯"-selected line text rather than pressing a fixed number of
// keys, so it lands on the right option whichever position it occupies (menu
// option order differs between Claude CLI versions). navKey is the arrow key
// to step with ("Down" or "Up"). Returns true once the option was confirmed
// or the screen is gone.
func (m *Manager) confirmMenuOption(agent *AgentProcess, title, want, navKey string) bool {
	const (
		// menuMaxNavigateSteps bounds arrow-key navigation; the handled menus
		// have 2 options, extra headroom covers future variants.
		menuMaxNavigateSteps = 4
		postKeystrokeDelay   = 500 * time.Millisecond
	)
	for step := 0; step < menuMaxNavigateSteps; step++ {
		pane := m.captureVisiblePaneForAgent(agent)
		if !strings.Contains(pane, title) || strings.Contains(pane, cliWorkingMarker) {
			return true // screen already dismissed
		}
		if strings.Contains(selectedMenuOption(pane), want) {
			m.tmuxSendKeysForAgent(agent, "Enter")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			return true
		}
		m.tmuxSendKeysForAgent(agent, navKey)
		m.sleepDuringPromptDismiss(postKeystrokeDelay)
	}
	m.logger.Warn("inference menu: wanted option not reached",
		"agent", agent.Name, "title", title, "want", want)
	return false
}

const (
	// consentStuckGracePeriod is how long a consent screen must stay visible
	// across watcher passes before the agent counts as stuck. The launch-time
	// dismissal goroutine runs for 60s, so a screen still visible this long
	// after first being seen by the watcher means dismissal lost the race.
	consentStuckGracePeriod = 30 * time.Second
	// consentDismissCooldown is the minimum interval between watcher-triggered
	// dismissal passes for one agent, so a stubborn screen can't spam
	// keystroke goroutines (each dismissal pass itself polls for 60s).
	consentDismissCooldown = 2 * time.Minute
)

// clearConsentTracking resets the consent-stuck timer for an agent whose pane
// no longer shows a consent screen.
func (m *Manager) clearConsentTracking(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.consentSeenAt = time.Time{}
	}
}

// dismissConsentIfStuck re-runs dismissInferencePrompts for an inference agent
// whose pane has shown a consent screen for longer than the grace period,
// subject to a per-agent cooldown. Called from the watcher loop
// (CheckAndRestartCrashedAgents) so an agent that lands on a consent screen
// after launch — e.g. a crash-recovery restart whose launch-time dismissal
// timed out — recovers instead of sitting stuck while kicks appear to succeed.
func (m *Manager) dismissConsentIfStuck(name string) {
	now := time.Now()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	if agent.consentSeenAt.IsZero() {
		agent.consentSeenAt = now
		m.mu.Unlock()
		return
	}
	stuckFor := now.Sub(agent.consentSeenAt)
	if stuckFor < consentStuckGracePeriod || now.Sub(agent.lastConsentDismiss) < consentDismissCooldown {
		m.mu.Unlock()
		return
	}
	agent.lastConsentDismiss = now
	m.mu.Unlock()

	m.logger.Warn("inference agent stuck on consent screen, re-running prompt dismissal",
		"name", name, "stuck_seconds", int(stuckFor.Seconds()))
	go m.dismissInferencePrompts(agent)
}

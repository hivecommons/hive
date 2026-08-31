package dashboard

import (
	"strings"
	"testing"
)

// TestTerminalCopyHints pins the usable fallback for issue #5188. Released
// ttyd does not consume OSC52 clipboard writes, and tmux mouse mode intercepts
// ordinary selection, so both dashboard agent layouts must explain
// Shift-selection next to their terminal controls. Login-blocked agents must
// keep the stronger URL warning even after the ordinary hint has been dismissed.
func TestTerminalCopyHints(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	for _, want := range []string{
		"const TERMINAL_COPY_HINT_LS_KEY = 'hive-terminal-copy-hint-dismissed';",
		"try { return localStorage.getItem(TERMINAL_COPY_HINT_LS_KEY) === '1'; }",
		"if (sessionUnavailable) return '';",
		"if (!needsLogin && terminalCopyHintDismissed()) {",
		"You’ll be copying a URL: hold <strong>Shift</strong> while selecting",
		"Terminal copy: <strong>⇧-drag</strong> to select",
		"const dismiss = needsLogin ? '' : '<button type=\"button\"",
		"try { localStorage.setItem(TERMINAL_COPY_HINT_LS_KEY, '1'); } catch {}",
		".terminal-copy-hint:not(.needs-login)",
		"data-action=\"dismissTerminalCopyHint\"",
		"${terminalCopyHintHtml(needsLoginDown, agentSessionUnavailable(a), a.name)}",
		"${terminalCopyHintHtml(needsLogin, agentSessionUnavailable(a), a.name)}",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("static dashboard terminal copy guidance is missing %q", want)
		}
	}

	if got := strings.Count(html, "${terminalCopyHintHtml("); got != 2 {
		t.Errorf("terminal copy hint rendered at %d call sites, want 2 (operator-console detail and legacy agent card)", got)
	}
	if strings.Contains(html, `onclick="dismissTerminalCopyHint`) {
		t.Error("terminal copy hint reintroduced an inline event handler; CSP requires data-action delegation")
	}
}

// TestTerminalCopyUrlButton pins the part of #5188 that actually completes a
// copy rather than explaining a workaround. The hint tells an operator how to
// work around the terminal; this button does the copy for them, and the
// contracts below are the ones whose loss would silently return the bug.
func TestTerminalCopyUrlButton(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	for _, want := range []string{
		// The control and its delegated, CSP-safe wiring.
		`data-action="copyTerminalUrl"`,
		"case 'copyTerminalUrl':",
		"/api/agents/${encodeURIComponent(agentName)}/terminal-urls",
		// Secure-context gate: navigator.clipboard must never be called
		// blind, because on a plain-http hive it rejects and the copy
		// silently does nothing — the original bug.
		"if (navigator.clipboard && window.isSecureContext) {",
		"ok = document.execCommand('copy');",
		// Every failure path must SAY something.
		"showToast(`Could not read ${agentName}'s terminal:",
		"showToast(`No URL on ${agentName}'s terminal right now`",
		"showTerminalUrlFallback(agentName, url);",
		"Clipboard access needs HTTPS or localhost",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("terminal copy-URL control is missing %q", want)
		}
	}

	// The button must survive hint dismissal. Dismissing the advice must not
	// remove the only affordance that actually copies.
	if !strings.Contains(html, "hint-dismissed") {
		t.Error("dismissing the hint must retain the copy-URL button, not delete the row")
	}
	if strings.Contains(html, `onclick="copyTerminalUrl`) {
		t.Error("copy-URL button used an inline handler; CSP requires data-action delegation")
	}
	// The agent name reaches an HTML attribute and must be escaped there.
	if !strings.Contains(html, `data-agent="${esc(agentName)}"`) {
		t.Error("copy-URL button must escape the agent name it puts in data-agent")
	}
}

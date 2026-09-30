package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestTerminalCopyHints pins the usable fallback for issue #5188, as reshaped
// by #5327. Released ttyd does not consume OSC52 clipboard writes, and tmux
// mouse mode intercepts ordinary selection, so both dashboard agent layouts
// must still reach the Shift-selection advice — but it now lives in a tooltip
// on a small affordance instead of a full sentence of permanent chrome on
// every agent card. Login-blocked agents keep the stronger URL warning, which
// is never dismissible.
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
		"if (terminalCopyHintDismissed()) return '';",
		// #5327: the keyboard advice survives, but as tooltip text rather than
		// a rendered sentence. Losing the ⇧/Ctrl+Shift+C content entirely
		// would strand operators on a terminal they cannot select out of.
		"const TERMINAL_KEYBOARD_COPY_HINT = 'Selecting text in the terminal: hold ⇧ (Shift) while dragging",
		"Ctrl+Shift+C copies and Ctrl+Shift+V pastes.",
		`title="${esc(TERMINAL_KEYBOARD_COPY_HINT)}"`,
		// The login-blocked row only claims a URL after the pane extractor found
		// one; otherwise it says the prompt has no visible URL.
		"This agent is waiting on a login.",
		"A login URL is visible on its terminal",
		"no login URL is visible on its terminal right now",
		"backend-auth-error",
		"Check the inference proxy / API key; this is not an interactive login.",
		"try { localStorage.setItem(TERMINAL_COPY_HINT_LS_KEY, '1'); } catch {}",
		".terminal-copy-hint:not(.needs-login)",
		"data-action=\"dismissTerminalCopyHint\"",
		"${terminalCopyHintHtml(needsLoginDown, agentSessionUnavailable(a), a.name, a.loginURL, a.backendAuthStatus, a.backendAuthLastError, a.statusEvidence)}",
		"${terminalCopyHintHtml(needsLogin, agentSessionUnavailable(a), a.name, a.loginURL, a.backendAuthStatus, a.backendAuthLastError, a.statusEvidence)}",
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

	// #5327: the ⇧-drag / Ctrl+Shift+C sentence must not come back as
	// always-on chrome. It was noise on every card, and welding an unrelated
	// copy control onto it is what made that control unreadable.
	if strings.Contains(html, "Terminal copy: <strong>⇧-drag</strong> to select") {
		t.Error("the always-visible keyboard hint sentence returned; #5327 moved it into a tooltip")
	}
	// A hover-only tooltip strands touch and keyboard users, so the
	// affordance must also be clickable.
	if !strings.Contains(html, `data-action="showTerminalKeyboardHint"`) {
		t.Error("keyboard-copy help must be reachable by click, not hover alone")
	}
}

func TestTerminalCopyHintBannerRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — terminal copy banner rendering was NOT executed by this run")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	script := `
function esc(v){ return String(v == null ? '' : v).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function terminalCopyHintDismissed(){ return false; }
function terminalCopyUrlButtonHtml(agentName, hasLoginURL){ if (!agentName || !hasLoginURL) return ''; return '<button data-action="copyTerminalUrl">Copy login URL</button>'; }
` + jsFunc(t, html, "terminalCopyHintHtml") + `
const withURL = terminalCopyHintHtml(true, false, 'scanner', 'https://github.com/login/device', '', '', '');
if (!withURL.includes('A login URL is visible on its terminal') || !withURL.includes('copyTerminalUrl')) throw new Error('login URL banner did not include copy control: '+withURL);
const noURL = terminalCopyHintHtml(true, false, 'scanner', '', '', '', '');
if (!noURL.includes('no login URL is visible') || noURL.includes('copyTerminalUrl') || noURL.includes('The URL is on its terminal')) throw new Error('no-URL login banner lied or showed copy control: '+noURL);
const backend = terminalCopyHintHtml(false, false, 'scanner', '', 'unreachable', 'Inference backend outage (401): Error querying the database: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute', '');
if (!backend.includes('backend-auth-error') || !backend.includes('not an interactive login') || backend.includes('copyTerminalUrl')) throw new Error('backend auth banner wrong: '+backend);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node terminal copy banner assertions failed: %v\n%s", err, out)
	}
}

// TestTerminalCopyUrlButton pins the part of #5188 that actually completes a
// copy rather than explaining a workaround, under the #5327 rules that make it
// legible: it is named for its case ("Copy login URL"), it is rendered only
// when a login is actually in flight, and it leads with a pre-selected field
// instead of a clipboard write that this deployment routinely blocks.
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
		"showToast(`No login URL on ${agentName}'s terminal right now`",
		"showTerminalUrlPanel(agentName, urls[0]);",
		"Automatic copying needs HTTPS or localhost",
		// An error toast is sticky and dismisses on any click within it, so
		// the fallback input must stop propagation or the first click into it
		// closes the toast and takes the URL away.
		"input.addEventListener('click', e => e.stopPropagation());",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("terminal copy-URL control is missing %q", want)
		}
	}

	// #5327 (1): the label must name the case, not the mechanism. An operator
	// asked "what is the 'copy URL' for?" precisely because it did not.
	if !strings.Contains(html, "Copy login URL</button>") {
		t.Error("copy control must be labelled for the login case it exists for, not the mechanism")
	}
	if strings.Contains(html, "🔗 copy URL") {
		t.Error("the mechanism-named 'copy URL' label returned; #5327 renamed it")
	}

	// #5327 (2): HIDDEN, not merely disabled, when no login is in flight.
	// needsLogin is the pane poller's own observation of a login prompt — the
	// same signal behind the 🔑 badge — so the control is present exactly when
	// there is a login URL worth offering.
	if !strings.Contains(html, "if (!agentName || !hasLoginURL) return '';") {
		t.Error("copy-URL button must be omitted entirely when no login URL is visible")
	}
	if !strings.Contains(html, "terminalCopyUrlButtonHtml(agentName, hasLoginURL)") {
		t.Error("copy-URL button must be passed the extracted-URL signal that gates it")
	}
	// It must offer the auth subset only. Falling back to the unfiltered list
	// is what handed the operator a repository URL under a login label.
	if !strings.Contains(html, "Array.isArray(d.authUrls) ? d.authUrls : []") {
		t.Error("copy control must consume authUrls, never the unfiltered URL list")
	}

	// #5327 (4): the pre-selected field is shown FIRST and the clipboard
	// write only reports its outcome, so a blocked write never becomes the
	// whole result of the operator's click.
	if !strings.Contains(html, "const ok = await writeClipboardText(url);") {
		t.Error("clipboard write must still be attempted underneath the field")
	}
	if !strings.Contains(html, "'✓ Also copied to your clipboard.'") {
		t.Error("a successful clipboard write must be reported as a bonus, not the headline")
	}
	// The field must be usable: an info toast self-dismisses on a timer, which
	// would take the URL away mid-read.
	if !strings.Contains(html, "if (toast) makeToastSticky(toast);") {
		t.Error("the login-URL panel must not self-dismiss while the operator is copying from it")
	}

	if strings.Contains(html, `onclick="copyTerminalUrl`) {
		t.Error("copy-URL button used an inline handler; CSP requires data-action delegation")
	}
	// The agent name reaches an HTML attribute and must be escaped there.
	if !strings.Contains(html, `data-agent="${esc(agentName)}"`) {
		t.Error("copy-URL button must escape the agent name it puts in data-agent")
	}
}

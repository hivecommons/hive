package panesignal

import "testing"

// claudeSecurityNotesPane is Claude Code v2.1.325's first-run security notes
// screen, verbatim from a subscription agent whose HOME had never run Claude
// Code (note the "…" ellipsis character in the footer).
const claudeSecurityNotesPane = `
 Security notes:

 1. Claude can make mistakes.
    You're responsible for Claude's actions and should always
    review them, especially when running code.

 2. Due to prompt injection risks, only use it with code you trust
    Learn more: https://code.claude.com/docs/en/security

 Press Enter to continue…
`

// TestBlockingPromptKey_ClaudeSecurityNotesPressesEnter: the screen is
// recognised for claude and answered with Enter alone (empty key).
func TestBlockingPromptKey_ClaudeSecurityNotesPressesEnter(t *testing.T) {
	key, label, ok := BlockingPromptKey("claude", claudeSecurityNotesPane)
	if !ok {
		t.Fatal("security notes screen not recognised; the startup kick would wait on it until it timed out")
	}
	if key != "" {
		t.Errorf("key = %q, want \"\" (Enter alone continues)", key)
	}
	if label != ClaudeSecurityNotesLabel {
		t.Errorf("label = %q, want %q", label, ClaudeSecurityNotesLabel)
	}
	if !HasBlockingPrompt(claudeSecurityNotesPane) {
		t.Error("readiness gate must hold while the security notes are on screen")
	}
	if ShowsClaudePermissionDialog(claudeSecurityNotesPane) {
		t.Error("the numbered security notes must not read as a permission dialog (option 1 would be typed)")
	}
	if _, _, ok := BlockingPromptKey("copilot", claudeSecurityNotesPane); ok {
		t.Error("claude security notes matched for a backend that does not render them")
	}
}

// TestBlockingPromptKey_ClaudeSecurityNotesQuotedIsNotAModal: Enter is the
// answer, so text that merely quotes the screen must never match. Each pane
// below holds both the header and the footer text.
func TestBlockingPromptKey_ClaudeSecurityNotesQuotedIsNotAModal(t *testing.T) {
	cases := map[string]string{
		// A kick quoting the screen, echoed into the transcript, with the CLI
		// back at its input box below it.
		"kick in transcript": `> The reviewer pane showed this:
  Security notes:
  Press Enter to continue…

● Noted.

────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`,
		// The same text still being typed into the input box.
		"kick in input box": `────────────────────────────────────────────────────────
> The reviewer pane showed this:
  Security notes:
  Press Enter to continue…
────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`,
		// The CLI exited and the kick was typed into bash, line by line.
		"kick in bash": `hive@hive:/data/agents/reviewer$ Security notes:
-bash: Security: command not found
hive@hive:/data/agents/reviewer$ Press Enter to continue…
-bash: Press: command not found
hive@hive:/data/agents/reviewer$`,
		// A footer without the header: some other "Press Enter" screen.
		"footer only": " Some other notice\n\n Press Enter to continue…",
		// The header without the footer as the last line: Enter was already
		// pressed and the next screen is drawing under it.
		"header only": claudeSecurityNotesPane + "\n Loading…",
	}
	for name, pane := range cases {
		t.Run(name, func(t *testing.T) {
			if key, label, ok := BlockingPromptKey("claude", pane); ok && label == ClaudeSecurityNotesLabel {
				t.Fatalf("quoted text matched as the live screen (key %q)", key)
			}
			if ShowsClaudeSecurityNotes(pane) {
				t.Fatal("ShowsClaudeSecurityNotes matched quoted text")
			}
		})
	}
}

// TestBlockingPromptKey_ClaudeSecurityNotesThenTrust: once Enter has been
// pressed the trust dialog draws under the notes. The trust dialog's own
// answer (Down, then Enter) must win; a bare Enter there picks "No, exit".
func TestBlockingPromptKey_ClaudeSecurityNotesThenTrust(t *testing.T) {
	pane := claudeSecurityNotesPane + "\n" + claudeWorkspaceTrustPane
	key, label, ok := BlockingPromptKey("claude", pane)
	if !ok || label != "claude workspace trust" || key != "Down" {
		t.Fatalf("got (%q, %q, %v), want the workspace trust answer (Down, claude workspace trust)", key, label, ok)
	}
}

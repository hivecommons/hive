package agent

import "testing"

// codexIdlePane is a verbatim capture of a healthy, idle Codex 0.144.1 pane from
// the live daviddiaz "Visual Hive" hive (hive-oke, backend codex v0.144.1). The
// scanner agent sat here awaiting a kick: the "›" (U+203A) input caret is
// present with placeholder ghost-text, and NONE of the other backends' markers
// ("❯", "goose is ready", "> Enter to send", bob placeholders) appear — which
// is exactly why the pre-fix detector timed out and dropped every kick, leaving
// the advisory issue stale. This is the regression fixture for the fix.
const codexIdlePane = `╭──────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.144.1)                   │
│                                              │
│ model:     gpt-5.4 medium   /model to change │
│ directory: /data/agents/scanner              │
╰──────────────────────────────────────────────╯

  Tip: Use /permissions to control when Codex asks for confirmation.


› Improve documentation in @filename

  gpt-5.4 medium · /data/agents/scanner
`

// codexIdlePaneOtherPlaceholder is the OTHER placeholder ghost-text Codex shows
// at the same idle caret (captured live from the quality agent). The marker
// must match regardless of which suggestion Codex renders, so we do not key on
// the placeholder string itself, only the "›" caret.
const codexIdlePaneOtherPlaceholder = `  Tip: New Use /fast to enable our fastest inference with increased plan usage.


› Explain this codebase

  gpt-5.6-terra default · /data/agents/quality
`

// codexBusyPane approximates a Codex pane mid-turn: it is actively streaming a
// response and shows a working indicator, NOT the idle "›" input caret. Sending
// a kick here would interleave with the running turn, so it must NOT be treated
// as ready. Codex renders the "›" caret only when idle-awaiting-input.
const codexBusyPane = `  OpenAI Codex (v0.144.1)

• Working (12s · esc to interrupt)

  Reading pkg/agent/manager.go
  Analyzing the readiness detector...
`

// TestCodexInputPromptMarker pins the vendor-defined caret literal. Codex is a
// compiled Rust binary, so this string cannot be re-derived from a bundle; if a
// Codex upgrade changes the caret, codex readiness breaks silently (its kick is
// dropped after inputPromptTimeout) and this test is the tripwire.
func TestCodexInputPromptMarker(t *testing.T) {
	// U+203A SINGLE RIGHT-POINTING ANGLE QUOTATION MARK — distinct from the
	// U+276F "❯" the other TUIs and the consent menu use, so it never collides
	// with paneShowsConsentScreen's "❯"-selected-line check.
	if want := "›"; codexInputPromptMarker != want {
		t.Errorf("codexInputPromptMarker = %q (%U), want %q (%U)",
			codexInputPromptMarker, []rune(codexInputPromptMarker)[0],
			want, []rune(want)[0])
	}
	if codexInputPromptMarker == "❯" {
		t.Fatalf("codex caret must NOT be the consent-menu %q (U+276F)", "❯")
	}
	if codexProductMarker != "OpenAI Codex" {
		t.Errorf("codexProductMarker = %q, want %q", codexProductMarker, "OpenAI Codex")
	}
}

// TestCodexPaneReadiness proves the fix: a real idle Codex pane is READY, a
// mid-turn Codex pane is NOT, and nothing else changed.
func TestCodexPaneReadiness(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		// --- codex: the new behaviour (both live captures) ---
		{"codex idle pane with placeholder ghost-text", codexIdlePane, true},
		{"codex idle pane, other placeholder", codexIdlePaneOtherPlaceholder, true},
		{"bare codex caret line", "› ", true},

		// --- codex must NOT false-ready mid-turn ---
		{"codex busy/streaming pane is NOT ready", codexBusyPane, false},

		// --- regression: pre-existing backends unchanged ---
		{"claude/copilot/gemini arrow prompt still ready", "some output\n❯ ", true},
		{"goose ready banner still ready", "goose is ready", true},
		{"bob placeholder still ready", "  " + bobInputPlaceholder, true},
		{"empty pane still not ready", "", false},
		{"bare bash prompt still not ready", "user@host:~$ ", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsInputPrompt(tc.pane); got != tc.want {
				t.Errorf("paneShowsInputPrompt(...) = %v, want %v\npane:\n%s",
					got, tc.want, tc.pane)
			}
		})
	}
}

// TestCodexPaneHasCLIMarker guards the coarse CLI-presence path: an idle Codex
// pane must be recognised as "a CLI is running" by paneHasCLIMarker, otherwise
// waitForCLIReadyForAgent never sees a booted codex and drops its kick.
func TestCodexPaneHasCLIMarker(t *testing.T) {
	if !paneHasCLIMarker(codexIdlePane) {
		t.Error("paneHasCLIMarker must recognise an idle codex pane (caret + banner)")
	}
	// Both codex markers must be reachable individually.
	for _, marker := range []string{codexInputPromptMarker, codexProductMarker} {
		if !paneHasCLIMarker("prefix " + marker + " suffix") {
			t.Errorf("paneHasCLIMarker must recognise codex marker %q", marker)
		}
	}
}

// TestCodexCaretNotConsentScreen guards the OTHER half of the readiness chain:
// the codex idle caret must not be mistaken for a "❯"-selected consent menu,
// which would make waitForInputPromptForAgent skip a ready pane forever.
func TestCodexCaretNotConsentScreen(t *testing.T) {
	if paneShowsConsentScreen(codexIdlePane) {
		t.Error("an idle codex pane must NOT be classified as a consent screen")
	}
}

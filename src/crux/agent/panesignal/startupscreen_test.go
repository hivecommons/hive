package panesignal

import (
	"reflect"
	"strings"
	"testing"
)

// sessionClaudeFirstRunNoticePane is Claude Code's first-run security notice,
// one line per row, as a live pane rendered it.
const sessionClaudeFirstRunNoticePane = ` Security notes:

 1. Claude can make mistakes
    You should always review Claude's responses, especially when
    running code.

 2. Due to prompt injection risks, only use it with code you trust
    For more details see:
    https://code.claude.com/docs/en/security

 Press Enter to continue…`

// sessionClaudeThemePickerPane is the first onboarding screen of Claude Code
// in a HOME that never completed onboarding.
const sessionClaudeThemePickerPane = ` Let's get started.

 Choose the text style that looks best with your terminal
 To change this later, run /theme

 ❯ 1. Dark mode ✔
   2. Light mode
   3. Dark mode (colorblind-friendly)
   4. Light mode (colorblind-friendly)`

// sessionClaudePermissionDialogPane is a Claude Code tool permission dialog,
// the prompt a session must never have answered for it.
const sessionClaudePermissionDialogPane = `╭──────────────────────────────────────────────────────────╮
│ Bash command                                             │
│                                                          │
│   rm -rf build                                           │
│                                                          │
│ Do you want to proceed?                                  │
│ ❯ 1. Yes                                                 │
│   2. Yes, and don't ask again for rm commands            │
│   3. No, and tell Claude what to do differently (esc)    │
╰──────────────────────────────────────────────────────────╯`

const sessionOmpOnboardingPane = `  Welcome to OMP
  A short tour of the basics follows.
  Press Enter to skip`

const sessionCopilotQuestionFormPane = `○ Asking user Waiting for response
Copilot needs information.
What would you like me to actually do right now?
❯
enter accept · tab next · ctrl+d decline · esc cancel`

// TestSessionStartupScreen_AnswersStartupScreens: each vendor's startup
// screens are recognised for a session and answered with exactly the keys the
// agents' prompt watcher sends (the option, when there is one, then Enter).
func TestSessionStartupScreen_AnswersStartupScreens(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		pane    string
		label   string
		keys    []string
	}{
		{"claude workspace trust", "claude", claudeWorkspaceTrustPane, "claude workspace trust", []string{"Down", "Enter"}},
		{"claude theme picker", "claude", sessionClaudeThemePickerPane, ClaudeThemePickerLabel, []string{"Enter"}},
		{"copilot folder trust", "copilot", copilotTrustPane, "copilot folder trust", []string{"1", "Enter"}},
		{"codex directory trust", "codex", codexTrustPane, "codex directory trust", []string{"1", "Enter"}},
		{"codex folder access", "codex", codexFolderAccessPane, "codex directory trust", []string{"1", "Enter"}},
		// Never "1. Update now", the pre-selected option.
		{"codex update", "codex", codexUpdatePane, CodexUpdatePromptLabel, []string{"3", "Enter"}},
		{"codex update 0.156", "codex", codexUpdatePane0156, CodexUpdatePromptLabel, []string{"3", "Enter"}},
		{"agy project trust", "agy", agyTrustPane, "agy project trust", []string{"Enter"}},
		{"omp onboarding", "omp", sessionOmpOnboardingPane, "omp onboarding", []string{"Enter", "Enter"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SessionStartupScreen(c.backend, c.pane)
			if !ok {
				t.Fatal("startup screen not recognised; a person would have to answer it by hand")
			}
			if got.Refused {
				t.Fatalf("startup screen %q was refused", got.Label)
			}
			if got.Label != c.label {
				t.Errorf("label = %q, want %q", got.Label, c.label)
			}
			if !reflect.DeepEqual(got.Keys, c.keys) {
				t.Errorf("keys = %v, want %v", got.Keys, c.keys)
			}
		})
	}
}

// TestSessionStartupScreen_ClaudeFirstRunNotice: the first-run security notice
// has no options, so Enter alone continues. It is reached through the agents'
// table entry when there is one and through the plain notice rule otherwise.
func TestSessionStartupScreen_ClaudeFirstRunNotice(t *testing.T) {
	got, ok := SessionStartupScreen("claude", sessionClaudeFirstRunNoticePane)
	if !ok || got.Refused {
		t.Fatalf("first-run notice = (%+v, %v), want an answerable startup screen", got, ok)
	}
	if got.Label != StartupNoticeLabel && got.Label != "claude first-run security notice" {
		t.Errorf("label = %q", got.Label)
	}
	if !reflect.DeepEqual(got.Keys, []string{"Enter"}) {
		t.Errorf("keys = %v, want a bare Enter", got.Keys)
	}
}

// TestSessionStartupScreen_RefusesPermissionPrompts is the boundary: a prompt
// that approves work is reported as refused and carries no keys, for the
// vendor that draws it and for every other vendor.
func TestSessionStartupScreen_RefusesPermissionPrompts(t *testing.T) {
	cases := []struct {
		name  string
		pane  string
		label string
	}{
		{"claude permission dialog", sessionClaudePermissionDialogPane, ClaudePermissionDialogLabel},
		{"codex command approval", codexCommandApprovalPane, "codex command approval"},
		{"claude auto-mode notice", claudeAutoModeNoticePane, ClaudeAutoModeNoticeLabel},
		{"copilot question form", sessionCopilotQuestionFormPane, CopilotQuestionFormLabel},
	}
	backends := []string{"claude", "copilot", "codex", "goose", "agy", "bob", "pi", "aider", "opencode", "kilo", "omp", "openhands"}
	for _, c := range cases {
		for _, backend := range backends {
			t.Run(c.name+"/"+backend, func(t *testing.T) {
				got, ok := SessionStartupScreen(backend, c.pane)
				if !ok {
					t.Fatal("permission prompt not recognised; it must be reported so the tab can say what is waiting")
				}
				if !got.Refused {
					t.Fatalf("permission prompt %q is answerable in a session with keys %v", got.Label, got.Keys)
				}
				if len(got.Keys) != 0 {
					t.Errorf("a refused prompt carries keys %v", got.Keys)
				}
				if got.Label != c.label {
					t.Errorf("label = %q, want %q", got.Label, c.label)
				}
			})
		}
	}
}

// TestSessionStartupScreen_OnlyTheVendorsOwnScreens: a startup entry is
// matched only for the vendor that renders it, as for agents.
func TestSessionStartupScreen_OnlyTheVendorsOwnScreens(t *testing.T) {
	for name, pane := range map[string]string{
		"codex trust":  codexTrustPane,
		"codex update": codexUpdatePane,
		"agy trust":    agyTrustPane,
	} {
		if got, ok := SessionStartupScreen("claude", pane); ok {
			t.Errorf("%s matched for claude as %+v", name, got)
		}
	}
}

// TestSessionStartupScreen_NoticeIsOnlyAPlainNotice: "Press Enter to continue"
// is answered only on a screen that offers no choice and asks nothing, with
// the footer at the very bottom.
func TestSessionStartupScreen_NoticeIsOnlyAPlainNotice(t *testing.T) {
	if got, ok := SessionStartupScreen("goose", "Welcome.\n\nPress Enter to continue"); !ok || got.Label != StartupNoticeLabel || got.Refused {
		t.Fatalf("plain notice = (%+v, %v)", got, ok)
	}
	for name, pane := range map[string]string{
		"a highlighted option":  "Pick one\n❯ Install the update\n  Skip\nPress Enter to continue",
		"a numbered selection":  "Pick one\n› 1. Install the update\n  2. Skip\nPress Enter to continue",
		"a question":            "Share usage data with the vendor?\nPress Enter to continue",
		"footer scrolled away":  "Notice\nPress Enter to continue\n\nline one\nline two\nline three",
		"lower-case menu foot":  "Something\nPress enter to continue",
		"an empty pane":         "",
		"a ready prompt":        "❯ \n? for shortcuts",
		"a ready codex prompt":  "› Implement {feature}\n  gpt-5.6 default · /work",
		"text quoting a footer": "The docs say Press Enter to continue\n❯ ",
	} {
		if got, ok := SessionStartupScreen("goose", pane); ok {
			t.Errorf("%s: matched as %+v", name, got)
		}
	}
}

// TestSessionStartupLabels_ClassifyEveryBlockingPrompt: every prompt the
// agents answer is explicitly a session startup screen or explicitly refused,
// never both, so a prompt added for agents is not answerable in a session
// until someone decides it is a startup screen.
func TestSessionStartupLabels_ClassifyEveryBlockingPrompt(t *testing.T) {
	for _, p := range BlockingPrompts {
		if !SessionStartupLabelKnown(p.label) {
			t.Errorf("BlockingPrompts label %q is not classified for sessions: add it to sessionStartupLabels (a startup screen) or sessionRefusedLabels (never answered)", p.label)
		}
		if sessionStartupLabels[p.label] && sessionRefusedLabels[p.label] {
			t.Errorf("label %q is both a startup screen and refused", p.label)
		}
	}
	for label := range sessionStartupLabels {
		lower := strings.ToLower(label)
		for _, word := range []string{"permission", "approval"} {
			if strings.Contains(lower, word) {
				t.Errorf("startup label %q names a %s prompt", label, word)
			}
		}
	}
	for _, label := range []string{ClaudePermissionDialogLabel, "codex command approval", ClaudeAutoModeNoticeLabel} {
		if sessionStartupLabels[label] {
			t.Errorf("%q must never be a session startup screen", label)
		}
	}
}

func TestShowsNumberedSelection(t *testing.T) {
	for pane, want := range map[string]bool{
		sessionClaudeThemePickerPane:      true,
		codexUpdatePane:                   true,
		sessionClaudePermissionDialogPane: true,
		"❯ \n? for shortcuts":             false,
		"› Implement {feature}":           false,
		sessionClaudeFirstRunNoticePane:   false,
		"":                                false,
	} {
		if got := ShowsNumberedSelection(pane); got != want {
			t.Errorf("ShowsNumberedSelection(%q) = %v, want %v", pane, got, want)
		}
	}
}

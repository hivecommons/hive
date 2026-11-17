package panesignal

import (
	"regexp"
	"strings"
)

// Startup screens for INTERACTIVE sessions (the dashboard's Sessions feature).
//
// Agents answer everything in BlockingPrompts, because they run unattended
// with the vendor's permission-bypass flags. A session is the opposite: a
// person's terminal, launched with no bypass flag, whose permission prompts
// are meant to reach that person. So a session uses the SAME table and the
// same matchers, but only the entries that are startup screens: a first-run
// notice, the trust question for the directory the session was opened in, an
// onboarding or update menu. Every other entry (a tool or command permission
// prompt, a notice that resumes a held action) is refused here and reported,
// never answered.
//
// The split is a property of each label, kept in the two sets below. A test
// asserts that every BlockingPrompts label is in exactly one of them, so a
// prompt added for agents cannot become answerable in sessions by default.

// sessionStartupLabels are the BlockingPrompts entries a session may answer.
var sessionStartupLabels = map[string]bool{
	"claude workspace trust": true,
	// The first-run "Security notes … Press Enter to continue" screen. Agents
	// answer it under this label; a session also reaches it through
	// StartupNoticeLabel below when the table has no entry for it.
	"claude first-run security notice": true,
	"copilot folder trust":             true,
	"agy project trust":                true,
	"omp onboarding":                   true,
	"codex directory trust":            true,
	// Answered with "Skip until next version", never the pre-selected
	// "Update now" (see the table entry).
	CodexUpdatePromptLabel: true,
}

// sessionRefusedLabels are the BlockingPrompts entries a session never
// answers: they approve work, they are not part of starting the CLI.
var sessionRefusedLabels = map[string]bool{
	ClaudePermissionDialogLabel: true,
	"codex command approval":    true,
	// Enter on this notice continues a held tool call (it appears mid-run).
	ClaudeAutoModeNoticeLabel: true,
}

const (
	// ClaudeThemePickerLabel is the first onboarding screen of Claude Code in
	// a HOME that never completed onboarding. Enter keeps the highlighted
	// default. A session HOME is seeded so this screen is normally skipped;
	// the entry is the fallback.
	ClaudeThemePickerLabel  = "claude theme picker"
	claudeThemePickerMarker = "Choose the text style that looks best with your terminal"

	// StartupNoticeLabel is a plain notice that only asks for Enter: the
	// footer "Press Enter to continue" at the bottom of a screen that offers
	// no choice and asks no question.
	StartupNoticeLabel  = "press-enter notice"
	startupNoticeFooter = "Press Enter to continue"
	// startupNoticeFooterLines is how many trailing non-blank lines the footer
	// may sit in for the notice to count as live.
	startupNoticeFooterLines = 2

	// CopilotQuestionFormLabel is Copilot CLI's ask_user form.
	CopilotQuestionFormLabel = "copilot question form"

	startupKeyEnter = "Enter"
)

// selectionCursorRe matches a line that starts with a menu or prompt cursor
// ("❯ No, exit", "› 1. Update now", "> 2. Skip", or a bare "❯" input prompt).
var selectionCursorRe = regexp.MustCompile(`^[❯›>]`)

// numberedSelectionRe matches a numbered option under the cursor
// ("❯ 1. Dark mode").
var numberedSelectionRe = regexp.MustCompile(`^[❯›>]\s*[1-9][0-9]?\.\s+\S`)

// StartupScreen is what SessionStartupScreen found.
type StartupScreen struct {
	Label string
	// Keys are the tmux key names that answer the screen, in order. Empty when
	// Refused.
	Keys []string
	// Refused is true for a prompt a session must not answer.
	Refused bool
}

// answerKeys is the keystroke sequence the agents' prompt watcher sends for a
// BlockingPrompt key: the option (when there is one to pick), then Enter.
func answerKeys(key string) []string {
	if key == "" {
		return []string{startupKeyEnter}
	}
	return []string{key, startupKeyEnter}
}

// SessionStartupScreen reports the known screen an interactive session's CLI
// is showing, for the given vendor. ok is false when the pane shows none (the
// CLI is loading, ready, or on a screen nobody has described).
//
// Refusal is decided first and across EVERY backend's entries: a permission
// prompt is never answered, whichever vendor drew it and whatever else is on
// the pane.
func SessionStartupScreen(backend, pane string) (StartupScreen, bool) {
	tail := PaneTail(pane, BlockingPromptTailLines)
	if tail == "" {
		return StartupScreen{}, false
	}
	for _, p := range BlockingPrompts {
		if !sessionStartupLabels[p.label] && p.match(tail) {
			return StartupScreen{Label: p.label, Refused: true}, true
		}
	}
	if ShowsCopilotQuestionForm(tail) {
		return StartupScreen{Label: CopilotQuestionFormLabel, Refused: true}, true
	}
	for _, p := range BlockingPrompts {
		if p.backend == backend && sessionStartupLabels[p.label] && p.match(tail) {
			return StartupScreen{Label: p.label, Keys: answerKeys(p.key)}, true
		}
	}
	if backend == "claude" && showsClaudeThemePicker(tail) {
		return StartupScreen{Label: ClaudeThemePickerLabel, Keys: answerKeys("")}, true
	}
	if showsStartupNotice(tail) {
		return StartupScreen{Label: StartupNoticeLabel, Keys: answerKeys("")}, true
	}
	return StartupScreen{}, false
}

// showsClaudeThemePicker requires the picker's own sentence AND a numbered
// option under the cursor, so text that merely quotes the sentence is not it.
func showsClaudeThemePicker(tail string) bool {
	return strings.Contains(tail, claudeThemePickerMarker) && ShowsNumberedSelection(tail)
}

// showsStartupNotice reports a notice that only asks for Enter. Three things
// must hold: the footer is at the very bottom, no line starts with a cursor
// (a menu's highlighted option would be confirmed by Enter, and an input
// prompt under an old notice is not a notice), and no line asks a question
// (Enter would be an answer to it).
func showsStartupNotice(tail string) bool {
	if !strings.Contains(PaneTail(tail, startupNoticeFooterLines), startupNoticeFooter) {
		return false
	}
	for _, raw := range strings.Split(tail, "\n") {
		line := cleanDialogLine(raw)
		if selectionCursorRe.MatchString(line) || strings.HasSuffix(line, "?") {
			return false
		}
	}
	return true
}

// ShowsNumberedSelection reports whether the pane tail has a numbered option
// under a selection cursor: a menu is waiting for a choice, so the pane is
// not an input prompt however many prompt glyphs it contains.
func ShowsNumberedSelection(pane string) bool {
	for _, raw := range strings.Split(PaneTail(pane, BlockingPromptTailLines), "\n") {
		if numberedSelectionRe.MatchString(cleanDialogLine(raw)) {
			return true
		}
	}
	return false
}

// SessionStartupLabelKnown reports whether a BlockingPrompts label has been
// classified for sessions (as a startup screen or as refused).
func SessionStartupLabelKnown(label string) bool {
	return sessionStartupLabels[label] || sessionRefusedLabels[label]
}

package panesignal

import (
	"regexp"
	"strings"
)

// BlockingPrompt is a startup-blocking modal that must be answered with a
// SPECIFIC numbered option rather than a bare Enter or a generic
// navigate-away-from-"No" heuristic.
//
// The generic heuristic in dismissInferencePrompts steers away from options
// containing "no"/"exit" and otherwise confirms whatever is selected. That is
// wrong for menus whose DEFAULT selection is affirmative but harmful — most
// notably codex's update prompt, where the pre-selected option shells out to
// `npm install -g`. Answering those needs the exact key, so each entry names
// the one prompt it answers and nothing else is ever blind-fired at.
//
// This mirrors blockingPromptKey() in bin/contributor-relay.js, which solved
// the same problem contributor-side. The hub had no equivalent.
type BlockingPrompt struct {
	// backend this prompt belongs to. Prompts are matched only against the
	// backend that actually renders them, so a codex pattern can never fire at
	// a claude pane that happens to contain similar words.
	backend string
	// match reports whether this prompt is the one on screen. All conditions
	// must hold, so a prompt is only answered when positively identified.
	match func(pane string) bool
	key   string // the option to type before Enter
	label string // for the audit log
}

const (
	// CodexUpdatePromptLabel identifies the codex update menu in BlockingPrompts.
	CodexUpdatePromptLabel = "codex update prompt"
	// claudeAutoModeNoticeMarker / claudeAutoModeNoticeFooter identify Claude
	// Code's auto-mode classifier notice, verbatim from a live FMA pane
	// (#9945): "We're changing auto mode to no longer charge for classifier
	// requests in Claude Code. … Enter to continue · Esc to cancel". Both
	// must be present so a kick that merely mentions auto mode cannot match.
	claudeAutoModeNoticeMarker = "classifier requests in Claude Code"
	claudeAutoModeNoticeFooter = "Enter to continue"
	ClaudeAutoModeNoticeLabel  = "claude auto-mode classifier notice"
	// claudeAutoModeNoticeFooterLines is how many trailing non-blank lines
	// the footer may sit in for the notice to count as live (the footer
	// itself plus a possible status/spinner line under it).
	claudeAutoModeNoticeFooterLines = 2
)

var ompSplashSkipRe = regexp.MustCompile(`(?i)press (?:⏎|enter) to skip`)

var BlockingPrompts = []BlockingPrompt{
	{
		backend: "claude",
		// Claude Code: "Quick safety check: Is this a project you created or
		// one you trust?" defaults to "No, exit". If it is not answered before
		// the kick path runs, Claude exits and the prompt text lands in bash.
		match: func(p string) bool {
			return strings.Contains(p, "Is this a project you created or one you trust") &&
				strings.Contains(p, "Yes, I trust this folder")
		},
		key:   "Down",
		label: "claude workspace trust",
	},
	{
		backend: "claude",
		// Claude Code's auto-mode classifier notice (#9945). It appears
		// MID-RUN, at the first checked tool call, not at startup, so the
		// 60s dismissInferencePrompts window never sees it; only this
		// lifetime watcher can. Enter continues the held action and the
		// notice never returns in that session; Esc cancels the turn. New
		// inference sessions launch with CLAUDE_CODE_AUTO_MODE_SERVER=0 so
		// the notice is not shown at all — this entry rescues panes that
		// were already running when that landed, and any session whose
		// settings re-enable the server check.
		//
		// The footer must be on the LAST non-blank lines: once Enter is
		// pressed the CLI resumes and draws output below the notice, and the
		// lingering text must not read as a live modal (it would hold the
		// readiness gate and re-fire Enter every trustReanswerAfter).
		match: func(p string) bool {
			return strings.Contains(p, claudeAutoModeNoticeMarker) &&
				strings.Contains(PaneTail(p, claudeAutoModeNoticeFooterLines), claudeAutoModeNoticeFooter)
		},
		key:   "",
		label: ClaudeAutoModeNoticeLabel,
	},
	{
		backend: "claude",
		// Claude Code permission dialog ("Read outside the working
		// directories?", "Do you want to proceed?" over a 1./2./3./4. list).
		// Hive launches claude with --dangerously-skip-permissions, so this
		// only appears when that posture failed to take effect (a bare
		// `--model` once swallowed the flag); option 1 restores it. The
		// watcher additionally checks agentExpectsSkipPermissions before
		// typing, so an operator launch_cmd WITHOUT the flag is left alone.
		match: ShowsClaudePermissionDialog,
		key:   ClaudePermissionDialogKey,
		label: ClaudePermissionDialogLabel,
	},
	{
		backend: "copilot",
		// Copilot: "Confirm folder trust" → 1. Yes (THIS SESSION ONLY).
		//
		// Deliberately NOT "2. Yes, and remember": remembering makes the CLI
		// rewrite the SHARED ~/.copilot/config.json from its own in-memory
		// snapshot, which stomps every other agent's state in that file — traced
		// live on hivecommons/hive (2026-08-22): each agent's "remember" wiped
		// the others' trustedFolders entries, and one stale rewrite resurrected
		// a dead token over the operator's fresh login, which is why re-logins
		// never stuck. Session-only trust writes NOTHING; the watcher now runs
		// for the agent's whole lifetime, so every (re)launch gets re-answered
		// and persistence is unnecessary.
		match: func(p string) bool {
			return strings.Contains(p, "Confirm folder trust") || strings.Contains(p, "Do you trust the files")
		},
		key:   "1",
		label: "copilot folder trust",
	},
	{
		backend: "agy",
		// agy (Antigravity CLI): "Do you trust the contents of this project?"
		// An arrow-key menu whose affirmative option is ALREADY selected, so a
		// bare Enter is correct and there is no numbered option to type. It
		// blocks startup exactly like the codex/copilot trust dialogs.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this project")
		},
		key:   "",
		label: "agy project trust",
	},
	{
		backend: "omp",
		// OMP renders the splash footer with either a unicode or plain key
		// name. The provider setup wizard is a login screen, not this prompt.
		match: ompSplashSkipRe.MatchString,
		key:   "Enter",
		label: "omp onboarding",
	},
	{
		backend: "codex",
		// codex: older CLIs rendered "Do you trust the contents of this
		// directory?"; 0.156.1 renders "Folder access" with an
		// explicit "Trust this folder?" question and "Trust and continue"
		// option. Both choose option 1 to continue.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this directory") ||
				(strings.Contains(p, "Trust this folder? Codex can read, edit, and run files here, subject to your permission settings") &&
					strings.Contains(p, "Trust and continue"))
		},
		key:   "1",
		label: "codex directory trust",
	},
	{
		backend: "codex",
		// codex: "✨ Update available[!] x -> y" → 3. Skip until next version.
		//
		// Deliberately NOT "1. Update now", which is the PRE-SELECTED option:
		// it runs `npm install -g @openai/codex` as the unprivileged agent UID,
		// which fails with EACCES and takes the CLI down with it — on every
		// launch, indefinitely, until a human intervenes. Even where it could
		// succeed it is slow, needs network, can fail half-way, and drifts the
		// CLI version out from under the image.
		//
		// "Skip until next version" is chosen over a plain "Skip" because it
		// persists: a plain Skip re-prompts on the very next launch.
		match: func(p string) bool {
			return strings.Contains(p, "Update available") && strings.Contains(p, "Skip until next version")
		},
		key:   "3",
		label: CodexUpdatePromptLabel,
	},
	{
		backend: "codex",
		// codex command approval prompt. The hub path now launches with
		// --dangerously-bypass-approvals-and-sandbox, but this keeps already
		// wedged panes visible and lets the watcher make the same unattended
		// choice the launch posture declares.
		match: func(p string) bool {
			return strings.Contains(p, "Would you like to run the following command?") &&
				strings.Contains(p, "Yes, proceed")
		},
		key:   "1",
		label: "codex command approval",
	},
}

// BlockingPromptTailLines bounds how much of the pane a prompt may be matched
// in. captureTmuxPaneForAgent returns SCROLLBACK, not just the visible screen,
// so matching the whole capture answers prompts that have long since scrolled
// away: after a codex CLI died, its update menu stayed in history and the
// watcher typed "3" into the bash shell that replaced it, once every poll,
// forever. A live modal is always at the bottom of the pane, so only the tail
// is eligible.
const BlockingPromptTailLines = 25

// PaneTail returns the last n non-blank lines of a captured pane.
func PaneTail(pane string, n int) string {
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// BlockingPromptKey returns the keystroke that dismisses whatever known
// startup-blocking modal this backend currently has on screen, and whether one
// was recognised. Only the tail of the pane is considered — see
// BlockingPromptTailLines.
func BlockingPromptKey(backend, pane string) (key, label string, ok bool) {
	tail := PaneTail(pane, BlockingPromptTailLines)
	for _, p := range BlockingPrompts {
		if p.backend == backend && p.match(tail) {
			return p.key, p.label, true
		}
	}
	return "", "", false
}

// ShowsBlockingPrompt reports whether the pane is currently sitting on a
// known startup-blocking modal (folder trust, codex update, …) for the given
// backend. The login-detector uses it to stand down: a trust-wedged pane is
// NOT a login problem, and pausing the agent for it kills the very watcher
// that would answer the prompt — the deadlock that kept hivecommons/hive's
// copilot agents "sitting at login prompt" through every re-login (2026-08-22).
func ShowsBlockingPrompt(backend, pane string) bool {
	_, _, ok := BlockingPromptKey(backend, pane)
	return ok
}

// HasBlockingPrompt reports whether ANY known blocking prompt is on the
// pane, regardless of backend. Used by the readiness gate, which does not know
// the backend; the patterns are specific enough that a false positive only
// delays readiness rather than mis-firing a keystroke.
func HasBlockingPrompt(pane string) bool {
	tail := PaneTail(pane, BlockingPromptTailLines)
	for _, p := range BlockingPrompts {
		if p.match(tail) {
			return true
		}
	}
	return false
}

// BackendHasBlockingPrompts reports whether any known startup-blocking prompt
// belongs to this backend, and so whether the watcher is worth running for it.
//
// Derived from the table rather than hardcoded: the watcher used to be gated on
// `backend == "copilot"`, which is why codex agents were never rescued from
// their update menu even after the menu itself was understood. Adding a prompt
// for a new backend now enables the watcher for it automatically.
func BackendHasBlockingPrompts(backend string) bool {
	for _, p := range BlockingPrompts {
		if p.backend == backend {
			return true
		}
	}
	return false
}

const (
	// ClaudePermissionDialogLabel is the BlockingPrompts audit label.
	ClaudePermissionDialogLabel = "claude permission dialog"
	// ClaudePermissionDialogKey is the option typed before Enter: Claude's
	// permission dialogs list their affirmative, persistent choice first.
	ClaudePermissionDialogKey = "1"
	// BypassConsentTitle is the heading of the --dangerously-skip-permissions
	// consent screen.
	BypassConsentTitle = "Bypass Permissions mode"
	// APIKeyPromptTitle is the heading of the custom-API-key approval prompt.
	APIKeyPromptTitle = "Detected a custom API key"
	// CLIWorkingMarker is shown while Claude Code is actively processing a
	// request; a pane in this state is never a consent screen.
	CLIWorkingMarker = "esc to interrupt"
)

// claudePermissionQuestions are the question lines (lower-cased) that
// identify a permission dialog. At least one must be present alongside a
// numbered option list for the pane to count as a dialog.
var claudePermissionQuestions = []string{
	"outside the working director",
	"do you want to proceed?",
	"do you want to allow",
	"do you want to make this edit",
	"do you want to create",
	"do you want to run",
	"allow this tool",
}

// claudePermissionOptionRe matches a numbered option line, selected ("❯ 1.")
// or not ("  2."), after box-drawing borders are stripped.
var claudePermissionOptionRe = regexp.MustCompile(`^(?:❯\s*)?([1-9])\.\s+\S`)

// claudePermissionDialogExclusions are other claude modals that also render a
// "?" title over a numbered list and have their own, different answers
// (blockingPrompts / dismissInferencePrompts). They must never be read as a
// permission dialog, or option 1 would be typed into them.
var claudePermissionDialogExclusions = []string{
	BypassConsentTitle,
	"Is this a project you created or one you trust",
	APIKeyPromptTitle,
	"Update available",
}

// ClaudePermissionDialog reports whether the pane tail shows a Claude Code
// permission dialog and returns its first line (the dialog title) for logs
// and the kick error. Only the pane tail is considered: a dismissed dialog
// lingers in scrollback.
func ClaudePermissionDialog(pane string) (title string, ok bool) {
	if pane == "" || strings.Contains(pane, CLIWorkingMarker) {
		return "", false
	}
	tail := PaneTail(pane, BlockingPromptTailLines)
	for _, ex := range claudePermissionDialogExclusions {
		if strings.Contains(tail, ex) {
			return "", false
		}
	}
	var (
		question string
		options  int
		sawFirst bool
	)
	for _, raw := range strings.Split(tail, "\n") {
		line := cleanDialogLine(raw)
		if line == "" {
			continue
		}
		if m := claudePermissionOptionRe.FindStringSubmatch(line); m != nil {
			options++
			if m[1] == "1" {
				sawFirst = true
			}
			continue
		}
		if question == "" && strings.HasSuffix(line, "?") && isClaudePermissionQuestion(line) {
			question = line
		}
	}
	if question == "" || options < 2 || !sawFirst {
		return "", false
	}
	return question, true
}

func isClaudePermissionQuestion(line string) bool {
	lower := strings.ToLower(line)
	for _, q := range claudePermissionQuestions {
		if strings.Contains(lower, q) {
			return true
		}
	}
	return false
}

// cleanDialogLine strips the box-drawing border Claude Code draws around its
// dialogs ("│ Read outside … │") and surrounding whitespace.
func cleanDialogLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "│┃|")
	return strings.TrimSpace(line)
}

// ShowsClaudePermissionDialog is ClaudePermissionDialog as a predicate.
func ShowsClaudePermissionDialog(pane string) bool {
	_, ok := ClaudePermissionDialog(pane)
	return ok
}

package agent

import (
	"regexp"
	"strings"
)

// Claude Code permission dialogs (#claude-permission-dialog).
//
// Observed live on a production spoke (Claude Code 2.1.316): a reviewer agent
// whose model was "" launched as `claude --model --dangerously-skip-permissions
// …`, so `--model` swallowed the bypass flag and Claude ran in its default
// permission-prompting mode. Its first read of /data/policies/reviewer-queue.md
// from cwd /data/agents/reviewer raised
//
//	Read outside the working directories?
//	Claude wants to read /data/policies/reviewer-queue.md …
//	Do you want to proceed?
//	❯ 1. Yes, and keep allowing reads outside the working directories
//	  2. No, block
//	  3. No, ask each time
//	  4. Yes, ask each time
//	To disable this prompt, set permissions.blockReadsOutsideWorkingDirectories …
//
// which no human was attached to answer. The kick path's prompt detector did
// not know the dialog (has_question_form:false) and reported the generic
// "CLI did not reach input prompt" for 40+ minutes.
//
// Hive's claude agents always launch with --dangerously-skip-permissions, so a
// permission dialog on screen is by definition a launch posture failure, and
// answering option 1 ("Yes, and keep allowing…" / "Yes, allow…") merely
// restores what the launch flags already declared.

const (
	// claudePermissionDialogLabel is the blockingPrompts audit label and the
	// kick-undeliverable reason prefix.
	claudePermissionDialogLabel  = "claude permission dialog"
	claudePermissionDialogReason = "CLI blocked on permission dialog"
	// claudePermissionDialogKey is the option typed before Enter: Claude's
	// permission dialogs list their affirmative, persistent choice first.
	claudePermissionDialogKey = "1"
	// claudePermissionDialogAnswerAttempts bounds how many times one kick's
	// prompt wait auto-answers before giving up with the dialog reason.
	claudePermissionDialogAnswerAttempts = 3
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
	bypassConsentTitle,
	"Is this a project you created or one you trust",
	apiKeyPromptTitle,
	"Update available",
}

// claudePermissionDialog reports whether the pane tail shows a Claude Code
// permission dialog and returns its first line (the dialog title) for logs
// and the kick error. Only the pane tail is considered: a dismissed dialog
// lingers in scrollback.
func claudePermissionDialog(pane string) (title string, ok bool) {
	if pane == "" || strings.Contains(pane, cliWorkingMarker) {
		return "", false
	}
	tail := paneTail(pane, blockingPromptTailLines)
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

// paneShowsClaudePermissionDialog is claudePermissionDialog as a predicate.
func paneShowsClaudePermissionDialog(pane string) bool {
	_, ok := claudePermissionDialog(pane)
	return ok
}

// paneShowsQuestionForm reports whether the pane is blocked on ANY CLI
// question form a kick must not be typed into: Copilot's ask_user form or a
// Claude Code permission dialog.
func paneShowsQuestionForm(pane string) bool {
	return paneShowsCopilotQuestionForm(pane) || paneShowsClaudePermissionDialog(pane)
}

// claudePermissionDialogNotReadyReason is the kick-undeliverable reason for a
// pane parked on a permission dialog, naming the dialog so an operator reading
// the log or the dashboard busy reason sees WHAT blocked the CLI.
func claudePermissionDialogNotReadyReason(title string) string {
	return claudePermissionDialogReason + ": " + title
}

// agentExpectsSkipPermissions reports whether this agent's launch posture is
// --dangerously-skip-permissions, i.e. whether a permission dialog on screen
// is a launch failure hive may answer on the operator's behalf. Hive's own
// claude builders always pass the flag; an operator launch_cmd must carry it
// explicitly to opt in.
func agentExpectsSkipPermissions(agent *AgentProcess, backend string) bool {
	if backend != "claude" {
		return false
	}
	cmd := strings.TrimSpace(agent.Config.LaunchCmd)
	return cmd == "" || strings.Contains(cmd, "--dangerously-skip-permissions")
}

// answerClaudePermissionDialogForKick answers a permission dialog that is
// blocking a kick with option 1 and logs the dialog text at WARN so the
// launch-posture failure behind it is visible. Caller must NOT hold m.mu.
func (m *Manager) answerClaudePermissionDialogForKick(agent *AgentProcess, title string, attempt int) {
	m.logger.Warn("kick target pane is blocked on a Claude Code permission dialog; the agent launched without --dangerously-skip-permissions taking effect — answering option 1",
		"agent", agent.Name, "dialog", title, "option", claudePermissionDialogKey, "attempt", attempt)
	m.tmuxSendKeysForAgent(agent, claudePermissionDialogKey)
	m.sleepDuringPromptDismiss(enterDelay)
	m.tmuxSendKeysForAgent(agent, "Enter")
	m.sleepDuringPromptDismiss(tuiMenuEscapeDelay)
}

package agent

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// claudeReadOutsideDialogPane is the Claude Code 2.1.316 permission dialog a
// production reviewer agent sat on for 40+ minutes after a bare `--model`
// swallowed its --dangerously-skip-permissions flag.
const claudeReadOutsideDialogPane = `
 ▐▛███▜▌   Claude Code v2.1.316
▝▜█████▛▘  claude-opus-4-8 · API Usage Billing
  ▘▘ ▝▝    /data/agents/reviewer

╭──────────────────────────────────────────────────────────────────────────╮
│ Read outside the working directories?                                   │
│                                                                          │
│ Claude wants to read /data/policies/reviewer-queue.md, which is         │
│ outside the working directories for this session.                       │
│                                                                          │
│ Do you want to proceed?                                                  │
│ ❯ 1. Yes, and keep allowing reads outside the working directories       │
│   2. No, block                                                           │
│   3. No, ask each time                                                   │
│   4. Yes, ask each time                                                  │
│                                                                          │
│ To disable this prompt, set permissions.blockReadsOutsideWorkingDirectories │
│ Esc to cancel                                                            │
╰──────────────────────────────────────────────────────────────────────────╯`

// claudeProceedDialogPane is the generic tool-approval shape (no box border,
// different title) that the same detector must also recognise.
const claudeProceedDialogPane = `
● Bash(cat /data/policies/reviewer-queue.md)

Do you want to proceed?
❯ 1. Yes
  2. Yes, and don't ask again for cat commands in /data/agents/reviewer
  3. No, and tell Claude what to do differently (esc)
`

const claudeIdlePromptPane = `
● Done. Waiting for the next task.

❯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

func TestClaudePermissionDialogDetection(t *testing.T) {
	tests := []struct {
		name      string
		pane      string
		wantOK    bool
		wantTitle string
	}{
		{"read outside working directories", claudeReadOutsideDialogPane, true, "Read outside the working directories?"},
		{"generic do you want to proceed", claudeProceedDialogPane, true, "Do you want to proceed?"},
		{"idle prompt", claudeIdlePromptPane, false, ""},
		{"empty", "", false, ""},
		{"working marker suppresses", claudeReadOutsideDialogPane + "\n✻ Thinking… (esc to interrupt)", false, ""},
		{
			name: "bypass consent screen has its own answer",
			pane: "WARNING: Claude Code running in Bypass Permissions mode\nDo you want to proceed?\n❯ 1. No, exit\n  2. Yes, I accept\nEnter to confirm · Esc to exit",
		},
		{
			name: "workspace trust has its own answer",
			pane: "Is this a project you created or one you trust?\n❯ 1. Yes, I trust this folder\n  2. No, exit\nEnter to confirm",
		},
		{
			name: "kick text quoting the question without options",
			pane: "❯ When Claude asks \"Do you want to proceed?\" always answer yes.\n\n❯\n  ⏵⏵ bypass permissions on",
		},
		{
			name: "numbered list without a permission question",
			pane: "Plan:\n1. Read the policy\n2. Review PR 12\n\n❯\n  ⏵⏵ bypass permissions on",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, ok := claudePermissionDialog(tc.pane)
			if ok != tc.wantOK || title != tc.wantTitle {
				t.Fatalf("claudePermissionDialog = (%q, %v), want (%q, %v)", title, ok, tc.wantTitle, tc.wantOK)
			}
			if paneShowsQuestionForm(tc.pane) != tc.wantOK && !paneShowsCopilotQuestionForm(tc.pane) {
				t.Errorf("paneShowsQuestionForm = %v, want %v", paneShowsQuestionForm(tc.pane), tc.wantOK)
			}
		})
	}

	// The dialog is a known blocking prompt for claude (and so for the
	// lifetime watcher and the readiness gate) but never for other backends.
	key, label, ok := blockingPromptKey("claude", claudeReadOutsideDialogPane)
	if !ok || key != claudePermissionDialogKey || label != claudePermissionDialogLabel {
		t.Errorf("blockingPromptKey(claude) = (%q, %q, %v)", key, label, ok)
	}
	if _, _, ok := blockingPromptKey("codex", claudeReadOutsideDialogPane); ok {
		t.Error("claude permission dialog must not match the codex backend")
	}
	if !paneShowsConsentScreen(claudeReadOutsideDialogPane) {
		t.Error("readiness gate must not treat the dialog as a ready prompt")
	}
}

func TestAgentExpectsSkipPermissions(t *testing.T) {
	withFlag := &AgentProcess{Config: config.AgentConfig{LaunchCmd: "claude --model x --dangerously-skip-permissions"}}
	withoutFlag := &AgentProcess{Config: config.AgentConfig{LaunchCmd: "claude --model x"}}
	builtin := &AgentProcess{}
	if !agentExpectsSkipPermissions(builtin, "claude") || !agentExpectsSkipPermissions(withFlag, "claude") {
		t.Error("built-in claude launch and an override carrying the flag expect bypass")
	}
	if agentExpectsSkipPermissions(withoutFlag, "claude") || agentExpectsSkipPermissions(builtin, "codex") {
		t.Error("an override without the flag, or a non-claude backend, must not be auto-answered")
	}
}

// TestSendKickClaudePermissionDialog drives SendKick against a pane parked on
// the live dialog: with the bypass posture the dialog is answered with "1" +
// Enter and the kick lands; without it, or when the dialog will not go away,
// the kick fails naming the dialog instead of the generic prompt timeout.
func TestSendKickClaudePermissionDialog(t *testing.T) {
	tests := []struct {
		name          string
		launchCmd     string
		dialogAnswers int // answers after which the dialog closes; -1 = never
		wantAnswers   int
		wantDelivery  bool
		wantErr       string
	}{
		{"dialog answered then kick delivered", "", 1, 1, true, ""},
		{"dialog never closes names the dialog", "", -1, claudePermissionDialogAnswerAttempts, false, "CLI blocked on permission dialog: Read outside the working directories?"},
		{"launch_cmd without bypass is not auto-answered", "claude --model claude-opus-4-8", 1, 0, false, "CLI blocked on permission dialog: Read outside the working directories?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
			inputPromptTimeout = 5 * time.Second
			inputPromptPollInterval = time.Millisecond
			defer func() {
				inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
			}()
			origExists := tmuxSessionExists
			tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
			defer func() { tmuxSessionExists = origExists }()

			agent := &AgentProcess{
				Name:        "reviewer",
				Config:      config.AgentConfig{Backend: "claude", LaunchCmd: tc.launchCmd},
				State:       StateRunning,
				tmuxSession: "hive-reviewer",
			}
			var logBuf bytes.Buffer
			m := &Manager{
				agents:           map[string]*AgentProcess{agent.Name: agent},
				idToName:         map[string]string{agent.Name: agent.Name},
				logger:           slog.New(slog.NewTextHandler(&logBuf, nil)),
				kickLogDir:       t.TempDir(),
				kickLogRetention: defaultKickLogRetention,
				kickLogMaxBytes:  defaultKickLogMaxBytes,
			}

			var answers int
			var events []string
			ft := termSeams(m)
			ft.captureVisiblePane = func(*AgentProcess) string {
				if tc.dialogAnswers < 0 || answers < tc.dialogAnswers {
					return claudeReadOutsideDialogPane
				}
				return claudeIdlePromptPane
			}
			ft.capturePane = ft.captureVisiblePane
			ft.sendKeys = func(_ *AgentProcess, keys ...string) {
				for _, key := range keys {
					events = append(events, "key:"+key)
					if key == claudePermissionDialogKey {
						answers++
					}
				}
			}
			ft.sendLiteral = func(_ *AgentProcess, text string) {
				events = append(events, "literal:"+text)
			}
			ft.captureFullLog = func(*AgentProcess) (string, error) { return "", nil }
			ft.clearHistory = func(*AgentProcess) {}
			ft.sleepDuringPromptDismiss = func(time.Duration) {}

			err := m.SendKick(agent.Name, "review PR 2324")

			if answers != tc.wantAnswers {
				t.Errorf("dialog answers = %d, want %d; events=%v", answers, tc.wantAnswers, events)
			}
			if tc.wantAnswers > 0 {
				first := slices.Index(events, "key:"+claudePermissionDialogKey)
				if first < 0 || first+1 >= len(events) || events[first+1] != "key:Enter" {
					t.Errorf("option 1 must be followed by Enter; events=%v", events)
				}
				if !strings.Contains(logBuf.String(), "Read outside the working directories?") {
					t.Errorf("WARN must name the dialog text; log=%s", logBuf.String())
				}
			}
			delivered := slices.Contains(events, "literal:review PR 2324")
			if delivered != tc.wantDelivery {
				t.Errorf("kick typed = %v, want %v; events=%v", delivered, tc.wantDelivery, events)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("SendKick returned error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SendKick error = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), kickPromptNotReadyReason) {
				t.Errorf("error must name the dialog instead of the generic %q: %v", kickPromptNotReadyReason, err)
			}
			if agent.LastKick != nil {
				t.Error("undeliverable kick was recorded as delivered")
			}
			if agent.RestartCount != 0 || agent.HasLaunched {
				t.Errorf("permission dialog must not trigger the crashed-CLI restart path (restarts=%d launched=%v)", agent.RestartCount, agent.HasLaunched)
			}
		})
	}
}

package agent

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const copilotQuestionFormPane = ` ○ Asking user Waiting for response
 Copilot needs information.
 What would you like me to actually do right now?
 [Concrete task]  Target repo (if applicable)
 Concrete task
 e.g. 'Audit observability in hivecommons/pluk and file a telemetry issue' or 'Just explain the policy above'
 ❯
 enter accept · tab next · ctrl+d decline · esc cancel`

func TestPaneShowsCopilotQuestionForm(t *testing.T) {
	if !paneShowsCopilotQuestionForm(copilotQuestionFormPane) {
		t.Fatal("verbatim Copilot question form was not detected")
	}
	if paneShowsCopilotQuestionForm("────────\n❯\n────────\n idle   Copilot v1.0.0") {
		t.Fatal("idle Copilot prompt was classified as a question form")
	}
	if paneShowsCopilotQuestionForm(claudeWorkspaceTrustPane) {
		t.Fatal("Claude consent screen was classified as a Copilot question form")
	}
}

func TestSendKickDismissesCopilotQuestionFormBeforeTyping(t *testing.T) {
	origTimeout, origPoll := inputPromptTimeout, inputPromptPollInterval
	inputPromptTimeout = 500 * time.Millisecond
	inputPromptPollInterval = 10 * time.Millisecond
	defer func() {
		inputPromptTimeout, inputPromptPollInterval = origTimeout, origPoll
	}()

	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	defer func() { tmuxSessionExists = origExists }()

	agent := &AgentProcess{
		Name:        "telemetry",
		Config:      config.AgentConfig{Backend: "copilot"},
		State:       StateRunning,
		tmuxSession: "hive-telemetry",
	}
	m := &Manager{
		agents:           map[string]*AgentProcess{"telemetry": agent},
		idToName:         map[string]string{"telemetry": "telemetry"},
		logger:           discardLogger(),
		kickLogDir:       t.TempDir(),
		kickLogRetention: defaultKickLogRetention,
		kickLogMaxBytes:  defaultKickLogMaxBytes,
	}

	var escapes int
	var events []string
	ft := termSeams(m)
	ft.captureVisiblePane = func(*AgentProcess) string {
		if escapes < 2 {
			return copilotQuestionFormPane
		}
		return "────────\n❯\n────────\n idle   Copilot v1.0.0"
	}
	ft.capturePane = ft.captureVisiblePane
	ft.sendKeys = func(_ *AgentProcess, keys ...string) {
		for _, key := range keys {
			events = append(events, "key:"+key)
			if key == "Escape" {
				escapes++
			}
		}
	}
	ft.sendLiteral = func(_ *AgentProcess, text string) {
		events = append(events, "literal:"+text)
	}
	ft.captureFullLog = func(*AgentProcess) (string, error) { return "", nil }
	ft.clearHistory = func(*AgentProcess) {}
	ft.sleepDuringPromptDismiss = func(time.Duration) {}

	if err := m.SendKick("telemetry", "audit telemetry now"); err != nil {
		t.Fatalf("SendKick returned error: %v", err)
	}

	if escapes != 2 {
		t.Fatalf("Escape count = %d, want 2", escapes)
	}
	if !slices.Contains(events, "literal:audit telemetry now") {
		t.Fatalf("kick text was not delivered after dismissing question form; events=%v", events)
	}
	literalAt := slices.Index(events, "literal:audit telemetry now")
	beforeLiteral := strings.Join(events[:literalAt], "\n")
	if strings.Contains(beforeLiteral, "key:Enter") {
		t.Fatalf("question form was accepted with Enter before kick delivery; events=%v", events)
	}
	if !slices.Equal(events[:2], []string{"key:Escape", "key:Escape"}) {
		t.Fatalf("first keys = %v, want two Escape dismissals", events[:2])
	}
}

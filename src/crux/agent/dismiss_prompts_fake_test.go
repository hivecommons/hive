package agent

import (
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

const readyPromptPane = ": esc to interrupt"

type scriptedPromptPane struct {
	mu             sync.Mutex
	initialPane    string
	afterDownPane  string
	readyAfterKeys int
	keys           []string
}

func newDismissPromptHarness(t *testing.T, name, initialPane string, readyAfterKeys int) (*Manager, *AgentProcess, *scriptedPromptPane) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{name: {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents[name]
	m.mu.RUnlock()

	script := &scriptedPromptPane{initialPane: initialPane, readyAfterKeys: readyAfterKeys}
	termSeams(m).captureVisiblePane = script.capture
	termSeams(m).sendKeys = script.sendKeys
	termSeams(m).sleepDuringPromptDismiss = func(time.Duration) { runtime.Gosched() }
	m.promptDismissTimeout = 200 * time.Millisecond
	return m, agent, script
}

func (s *scriptedPromptPane) capture(*AgentProcess) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) >= s.readyAfterKeys {
		return readyPromptPane
	}
	if s.afterDownPane != "" && slices.Contains(s.keys, "Down") {
		return s.afterDownPane
	}
	return s.initialPane
}

func (s *scriptedPromptPane) sendKeys(_ *AgentProcess, keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, keys...)
}

func (s *scriptedPromptPane) keySnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

func runDismissPromptScript(t *testing.T, m *Manager, agent *AgentProcess, script *scriptedPromptPane, wantKeys []string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		m.dismissInferencePrompts(agent)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dismissInferencePrompts should return")
	}
	if got := script.keySnapshot(); !slices.Equal(got, wantKeys) {
		t.Fatalf("dismissInferencePrompts keys = %v, want %v", got, wantKeys)
	}
}

const bypassConsentKickPane = "WARNING: Claude Code running in Bypass Permissions mode\n❯ 1. No, exit\n  2. Yes, I accept\nEnter to confirm · Esc to exit"

// TestAcceptBypassConsentForKick_AnswersScreen is the #14307 guard: a kick that
// finds the bypass consent screen accepts it in place, instead of restarting
// the CLI into the same screen forever.
func TestAcceptBypassConsentForKick_AnswersScreen(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "kickconsent", bypassConsentKickPane, 2)
	script.afterDownPane = "WARNING: Claude Code running in Bypass Permissions mode\n  1. No, exit\n❯ 2. Yes, I accept\nEnter to confirm · Esc to exit"

	if !m.acceptBypassConsentForKick(agent, bypassConsentKickPane) {
		t.Fatal("acceptBypassConsentForKick should answer the bypass consent screen")
	}
	if got, want := script.keySnapshot(), []string{"Down", "Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

// TestAcceptBypassConsentForKick_LeavesOtherScreensToRestart: only the bypass
// consent screen is answered here; a bare shell or another menu keeps the
// existing restart recovery and receives no keystrokes.
func TestAcceptBypassConsentForKick_LeavesOtherScreensToRestart(t *testing.T) {
	for name, pane := range map[string]string{
		"bare shell":    "dev@hive:~$ ",
		"other menu":    ": Enter to confirm\n❯ 1. No, exit",
		"title only":    "Bypass Permissions mode\n❯ No, exit",
		"empty capture": "",
	} {
		t.Run(name, func(t *testing.T) {
			m, agent, script := newDismissPromptHarness(t, "kickother", pane, 99)
			if m.acceptBypassConsentForKick(agent, pane) {
				t.Fatal("acceptBypassConsentForKick should decline a non-bypass screen")
			}
			if got := script.keySnapshot(); len(got) != 0 {
				t.Fatalf("keys = %v, want none", got)
			}
		})
	}
}

// TestAcceptBypassConsentForKick_FallsBackWhenNotAccepted: if the accept option
// is never reached the helper reports failure so the caller restarts as before.
func TestAcceptBypassConsentForKick_FallsBackWhenNotAccepted(t *testing.T) {
	m, agent, script := newDismissPromptHarness(t, "kickstuck", bypassConsentKickPane, 99)
	if m.acceptBypassConsentForKick(agent, bypassConsentKickPane) {
		t.Fatal("acceptBypassConsentForKick should report failure when the screen never clears")
	}
	for _, k := range script.keySnapshot() {
		if k == "Enter" {
			t.Fatal("must never confirm while \"No, exit\" is still selected")
		}
	}
}

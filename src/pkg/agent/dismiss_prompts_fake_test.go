package agent

import (
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
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
	m.terminal = funcTerminal{
		captureVisiblePane:       script.capture,
		sendKeys:                 script.sendKeys,
		sleepDuringPromptDismiss: func(time.Duration) { runtime.Gosched() },
	}
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

package agent

// Test for the #3919 root-cause fix in tmuxSendEntersForAgent: the repeated
// "insurance" Enters exist only to make sure a typed line ran, and must STOP
// once a known blocking prompt is on screen — an Enter at that point is no
// longer insurance, it is an ANSWER, and codex's update menu pre-selects the
// destructive option ("1. Update now" → npm install -g as the agent UID →
// EACCES → dead CLI, on every launch).

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"log/slog"

	"github.com/kubestellar/hive/pkg/config"
)

// syncBuffer is a bytes.Buffer safe for a logger that may be written from
// another goroutine (the race detector runs in CI).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// entersGuardManager builds a manager whose visible-pane seam serves the given
// pane content and whose logger records into the returned buffer. The agent's
// session does not exist, so the raw Enter sends are harmless no-ops — the
// behaviour under test is the guard's early return, observable in the log.
func entersGuardManager(t *testing.T, pane string) (*Manager, *AgentProcess, *syncBuffer) {
	t.Helper()
	logBuf := &syncBuffer{}
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("codex", "gpt-5-codex"),
	}, slog.New(slog.NewTextHandler(logBuf, nil)), ProjectContext{})
	m.terminal = funcTerminal{captureVisiblePane: func(*AgentProcess) string { return pane }}
	m.mu.RLock()
	agent := m.agents["worker"]
	m.mu.RUnlock()
	return m, agent, logBuf
}

// TestTmuxSendEnters_WithholdsInsuranceEntersOnBlockingPrompt: with codex's
// update menu on screen, the FIRST Enter is still sent (it is what runs the
// typed line) but the insurance repeats are withheld — the guard fires on the
// second iteration having sent exactly one.
func TestTmuxSendEnters_WithholdsInsuranceEntersOnBlockingPrompt(t *testing.T) {
	m, agent, logBuf := entersGuardManager(t, codexUpdatePane)

	m.tmuxSendEntersForAgent(agent)

	out := logBuf.String()
	if !strings.Contains(out, "stopping repeat Enter") {
		t.Fatalf("guard did not fire with the codex update menu on screen — Enters #2/#3 would confirm the pre-selected 'Update now'; log = %q", out)
	}
	if !strings.Contains(out, "sent=1") {
		t.Errorf("guard fired after the wrong number of Enters (want exactly the first, insurance withheld); log = %q", out)
	}
}

// TestTmuxSendEnters_NoPromptSendsAllInsurance is the positive control: with
// nothing awaiting an answer, the insurance repeats must keep flowing — a
// guard that fires on an ordinary pane would reintroduce the swallowed-Enter
// launches the repeats exist to prevent.
func TestTmuxSendEnters_NoPromptSendsAllInsurance(t *testing.T) {
	m, agent, logBuf := entersGuardManager(t, "  ordinary CLI output\n❯ ")

	m.tmuxSendEntersForAgent(agent)

	if out := logBuf.String(); strings.Contains(out, "stopping repeat Enter") {
		t.Errorf("guard fired without a blocking prompt on screen; log = %q", out)
	}
}

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// diagnosticKeystrokes records what runCopilotDiagnostic types into the pane
// through the terminal seam.
type diagnosticKeystrokes struct {
	mu    sync.Mutex
	typed []string
}

func (k *diagnosticKeystrokes) add(s string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.typed = append(k.typed, s)
}

func (k *diagnosticKeystrokes) snapshot() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.typed...)
}

func newDiagnosticTestManager(t *testing.T, session string) (*Manager, *AgentProcess, *diagnosticKeystrokes) {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxdiag": {Backend: "copilot"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "cxdiag")
	m.mu.RLock()
	agent := m.agents["cxdiag"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	t.Cleanup(func() { _ = m.tmuxCmd(agent, "kill-session", "-t", session).Run() })
	return m, agent, &diagnosticKeystrokes{}
}

// TestRunCopilotDiagnostic_CancelledContextLeavesSessionAlone pins the live
// knuckle failure (2026-11-08): a poller tick still in flight when a session
// recycle restarted the agent started the diagnostic with the poller's
// already-cancelled ctx. The diagnostic then interrupted and killed the
// relaunched session, typed a bare `copilot` (no --allow-all, no --model),
// and returned at once on ctx.Done, leaving the agent on a CLI that asks
// approval for every shell command.
//
// On v6 tip the C-c and the bare-copilot line are typed before ctx is ever
// looked at, so this fails; with the fix the diagnostic types nothing.
func TestRunCopilotDiagnostic_CancelledContextLeavesSessionAlone(t *testing.T) {
	m, agent, keys := newDiagnosticTestManager(t, "hive-cxdiag-cancelled")
	ft := termSeams(m)
	ft.sendKeys = func(_ *AgentProcess, k ...string) { keys.add("keys:" + strings.Join(k, " ")) }
	ft.sendLiteral = func(_ *AgentProcess, text string) { keys.add("literal:" + text) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.runCopilotDiagnostic(ctx, agent)

	if typed := keys.snapshot(); len(typed) != 0 {
		t.Fatalf("diagnostic for a superseded launch typed into the pane: %q", typed)
	}
	if !m.mu.TryLock() {
		t.Fatal("diagnostic returned with m.mu still held")
	}
	m.mu.Unlock()
}

// TestRunCopilotDiagnostic_RestartDuringSettleDoesNotLaunchBareCopilot covers
// the narrower window: the hang is real when the diagnostic starts, but a
// restart cancels the launch while the diagnostic waits for the C-c to land.
// The restart owns the session from then on, so the diagnostic must not kill
// it or type bare `copilot` into its replacement.
//
// On v6 tip nothing re-checks ctx after the C-c, so the bare-copilot line is
// typed (this needs a real tmux for ensureTmuxSession to succeed); with the fix
// the second check under m.mu returns before the kill.
func TestRunCopilotDiagnostic_RestartDuringSettleDoesNotLaunchBareCopilot(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m, agent, keys := newDiagnosticTestManager(t, "hive-cxdiag-settle")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ft := termSeams(m)
	ft.sendKeys = func(_ *AgentProcess, k ...string) {
		keys.add("keys:" + strings.Join(k, " "))
		if len(k) > 0 && k[0] == "C-c" {
			// What restartWithReason does: cancel the per-launch context
			// (it does so under m.mu, which the diagnostic holds here, so
			// cancelling directly stands in for "the restart ran next").
			cancel()
		}
	}
	ft.sendLiteral = func(_ *AgentProcess, text string) { keys.add("literal:" + text) }

	m.runCopilotDiagnostic(ctx, agent)

	for _, s := range keys.snapshot() {
		if strings.HasPrefix(s, "literal:") && strings.Contains(s, "copilot") {
			t.Fatalf("diagnostic typed bare copilot into a session a restart now owns: %q", keys.snapshot())
		}
	}
	if !m.mu.TryLock() {
		t.Fatal("diagnostic returned with m.mu still held")
	}
	m.mu.Unlock()
}

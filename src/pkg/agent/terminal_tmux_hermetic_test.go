package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// installHermeticTmuxTerminal installs a fake tmux on PATH that answers the
// two tmuxTerminal calls not exercised by the lifecycle fake:
//
//   - display-message  → prints HIVE_FAKE_TMUX_ATTACHED (default "1"), exits
//     HIVE_FAKE_TMUX_DISPLAY_EXIT (default 0)
//   - clear-history    → logs the full command line and exits 0
//
// Every invocation is appended to the returned log so tests can assert the
// exact tmux command the production tmuxTerminal built.
func installHermeticTmuxTerminal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
if [ -n "${HIVE_FAKE_TMUX_TERMINAL_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$HIVE_FAKE_TMUX_TERMINAL_LOG"
fi
case "$*" in
  *display-message*) printf '%s' "${HIVE_FAKE_TMUX_ATTACHED:-1}"; exit "${HIVE_FAKE_TMUX_DISPLAY_EXIT:-0}" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HIVE_FAKE_TMUX_TERMINAL_LOG", logPath)
	return logPath
}

func newTmuxTerminalUnderTest(t *testing.T) (tmuxTerminal, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"worker": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["worker"]
	m.mu.RUnlock()
	agent.tmuxSession = "hive-terminal-test"
	agent.UID = 0 // plain tmux path: no su-exec wrapper
	return tmuxTerminal{m: m}, agent
}

// SessionAttached must fail OPEN (report attached) on every uncertain path:
// nil agent, no session name, tmux error, and unparseable output. Only a
// well-formed "0" may report detached.
func TestTmuxTerminalSessionAttached(t *testing.T) {
	logPath := installHermeticTmuxTerminal(t)
	term, agent := newTmuxTerminalUnderTest(t)

	if !term.SessionAttached(nil) {
		t.Error("nil agent must fail open (attached)")
	}
	if !term.SessionAttached(&AgentProcess{Name: "no-session"}) {
		t.Error("empty tmuxSession must fail open (attached)")
	}

	t.Setenv("HIVE_FAKE_TMUX_ATTACHED", "1")
	if !term.SessionAttached(agent) {
		t.Error("session_attached=1 must report attached")
	}
	t.Setenv("HIVE_FAKE_TMUX_ATTACHED", "0")
	if term.SessionAttached(agent) {
		t.Error("session_attached=0 must report detached")
	}
	t.Setenv("HIVE_FAKE_TMUX_ATTACHED", "not-a-number")
	if !term.SessionAttached(agent) {
		t.Error("unparseable tmux output must fail open (attached)")
	}
	t.Setenv("HIVE_FAKE_TMUX_ATTACHED", "0")
	t.Setenv("HIVE_FAKE_TMUX_DISPLAY_EXIT", "1")
	if !term.SessionAttached(agent) {
		t.Error("tmux command failure must fail open (attached)")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "display-message -p -t hive-terminal-test #{session_attached}") {
		t.Fatalf("fake tmux did not receive the display-message query:\n%s", raw)
	}
}

// ClearHistory must issue exactly one clear-history against the agent's
// session and leave the pane alone (no capture-pane, no send-keys).
func TestTmuxTerminalClearHistory(t *testing.T) {
	logPath := installHermeticTmuxTerminal(t)
	term, agent := newTmuxTerminalUnderTest(t)

	term.ClearHistory(agent)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(raw)
	if got := strings.Count(logged, "clear-history -t hive-terminal-test"); got != 1 {
		t.Fatalf("clear-history sent %d times, want 1:\n%s", got, logged)
	}
	for _, forbidden := range []string{"capture-pane", "send-keys"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("ClearHistory must not run %s:\n%s", forbidden, logged)
		}
	}
}

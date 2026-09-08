package agent

// Hermetic coverage for the two production tmuxTerminal methods that had
// none: SessionAttached (the attach gate behind
// Manager.tmuxSessionHasAttachedClientForAgent, manager.go) and ClearHistory
// (invoked by the kick-log capture path, kick_logs.go). SessionAttached's
// contract is "fail open" — when tmux is unreachable or answers garbage the
// Manager must assume a human is attached — so every fallback branch is
// pinned here against a fake tmux on PATH (same pattern as
// tmux_lifecycle_hermetic_test.go).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installAttachFakeTmux puts a fake tmux first on PATH whose display-message
// prints $HIVE_FAKE_TMUX_ATTACHED and exits $HIVE_FAKE_TMUX_DISPLAY_EXIT,
// and which appends every invocation to the returned log file.
func installAttachFakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HIVE_FAKE_TMUX_ATTACH_LOG"
case "$*" in
  *display-message*)
    printf '%s' "$HIVE_FAKE_TMUX_ATTACHED"
    exit "${HIVE_FAKE_TMUX_DISPLAY_EXIT:-0}"
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HIVE_FAKE_TMUX_ATTACH_LOG", logPath)
	return logPath
}

func TestTmuxTerminalSessionAttachedFailsOpenWithoutSession(t *testing.T) {
	// nil agent and empty tmuxSession never reach tmux at all — no fake on
	// PATH, so a real tmux invocation would fail the test environment.
	term := tmuxTerminal{m: NewManager(nil, discardLogger(), ProjectContext{})}
	if !term.SessionAttached(nil) {
		t.Fatal("SessionAttached(nil) must fail open (true)")
	}
	if !term.SessionAttached(&AgentProcess{Name: "quality"}) {
		t.Fatal("SessionAttached with empty tmuxSession must fail open (true)")
	}
}

func TestTmuxTerminalSessionAttachedParsesClientCount(t *testing.T) {
	installAttachFakeTmux(t)
	term := tmuxTerminal{m: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-test"}

	cases := []struct {
		name   string
		output string
		exit   string
		want   bool
	}{
		{"no clients", "0\n", "0", false},
		{"one client", "1\n", "0", true},
		{"many clients with padding", "  2 \n", "0", true},
		{"tmux error fails open", "", "1", true},
		{"non-numeric output fails open", "not-a-number\n", "0", true},
		{"empty output fails open", "", "0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_FAKE_TMUX_ATTACHED", tc.output)
			t.Setenv("HIVE_FAKE_TMUX_DISPLAY_EXIT", tc.exit)
			if got := term.SessionAttached(agent); got != tc.want {
				t.Fatalf("SessionAttached with output %q exit %s = %v, want %v",
					tc.output, tc.exit, got, tc.want)
			}
		})
	}
}

func TestTmuxTerminalSessionAttachedReachesManagerSeam(t *testing.T) {
	// The Manager-level wrapper must consult the installed TerminalSession.
	installAttachFakeTmux(t)
	t.Setenv("HIVE_FAKE_TMUX_ATTACHED", "0\n")
	m := NewManager(nil, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-seam"}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		t.Fatal("wrapper should report detached when tmux says 0 clients")
	}
}

func TestTmuxTerminalClearHistorySendsClearHistoryCommand(t *testing.T) {
	logPath := installAttachFakeTmux(t)
	term := tmuxTerminal{m: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-clear-test"}

	term.ClearHistory(agent)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux was never invoked: %v", err)
	}
	if !strings.Contains(string(raw), "clear-history -t hive-clear-test") {
		t.Fatalf("ClearHistory sent %q, want clear-history -t hive-clear-test", string(raw))
	}
}

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
	"strconv"
	"strings"
	"testing"
	"time"
)

// installAttachFakeTmux puts a fake tmux first on PATH whose list-clients
// prints $HIVE_FAKE_TMUX_CLIENTS and exits $HIVE_FAKE_TMUX_CLIENTS_EXIT, whose
// display-message prints $HIVE_FAKE_TMUX_ATTACHED and exits
// $HIVE_FAKE_TMUX_DISPLAY_EXIT, and which appends every invocation to the
// returned log file.
func installAttachFakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HIVE_FAKE_TMUX_ATTACH_LOG"
case "$*" in
  *list-clients*)
    printf '%s' "$HIVE_FAKE_TMUX_CLIENTS"
    exit "${HIVE_FAKE_TMUX_CLIENTS_EXIT:-0}"
    ;;
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
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	if !term.SessionAttached(nil) {
		t.Fatal("SessionAttached(nil) must fail open (true)")
	}
	if !term.SessionAttached(&AgentProcess{Name: "quality"}) {
		t.Fatal("SessionAttached with empty tmuxSession must fail open (true)")
	}
}

// TestTmuxTerminalSessionAttachedIgnoresStaleClients covers the guard's whole
// contract, including the live failure it was changed for: two tmux clients
// abandoned on a hosted spoke's scanner session (client_activity frozen at
// client_created, no process owning either pty) kept every keyboard-based heal
// switched off for 13.5 hours.
//
// tmux reports client_activity as a unix timestamp, one line per client, and
// prints nothing at all when no client is attached.
func TestTmuxTerminalSessionAttachedIgnoresStaleClients(t *testing.T) {
	installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-test"}

	now := time.Now()
	recent := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	stale := strconv.FormatInt(now.Add(-attachedClientIdleGrace-time.Minute).Unix(), 10)
	justInside := strconv.FormatInt(now.Add(-attachedClientIdleGrace+time.Minute).Unix(), 10)

	cases := []struct {
		name   string
		output string
		exit   string
		want   bool
	}{
		{"no clients at all", "", "0", false},
		{"one active client", recent + "\n", "0", true},
		{"client just inside the grace still counts", justInside + "\n", "0", true},
		{"single abandoned client is ignored", stale + "\n", "0", false},
		{"two abandoned clients are ignored (the spoke incident)", stale + "\n" + stale + "\n", "0", false},
		{"one active among abandoned still blocks", stale + "\n" + recent + "\n", "0", true},
		{"tmux error fails open", "", "1", true},
		{"unparseable timestamp fails open", "not-a-number\n", "0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_FAKE_TMUX_CLIENTS", tc.output)
			t.Setenv("HIVE_FAKE_TMUX_CLIENTS_EXIT", tc.exit)
			if got := term.SessionAttached(agent); got != tc.want {
				t.Fatalf("SessionAttached with output %q exit %s = %v, want %v",
					tc.output, tc.exit, got, tc.want)
			}
		})
	}
}

// TestTmuxTerminalSessionAttachedQueriesClientActivity pins the tmux call
// itself: asking for a client COUNT is what made abandoned clients
// indistinguishable from a live operator, so the format string is the fix.
func TestTmuxTerminalSessionAttachedQueriesClientActivity(t *testing.T) {
	logPath := installAttachFakeTmux(t)
	t.Setenv("HIVE_FAKE_TMUX_CLIENTS", "")
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}

	_ = term.SessionAttached(&AgentProcess{Name: "quality", tmuxSession: "hive-activity-probe"})

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux was never invoked: %v", err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "list-clients -t hive-activity-probe -F #{client_activity}") {
		t.Fatalf("SessionAttached sent %q, want list-clients with -F #{client_activity}", logged)
	}
}

func TestTmuxTerminalSessionAttachedReachesManagerSeam(t *testing.T) {
	// The Manager-level wrapper must consult the installed TerminalSession.
	installAttachFakeTmux(t)
	t.Setenv("HIVE_FAKE_TMUX_CLIENTS", "")
	m := NewManager(nil, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-seam"}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		t.Fatal("wrapper should report detached when tmux says 0 clients")
	}
}

func TestTmuxTerminalCapturePaneJoinsWrappedLinesCommand(t *testing.T) {
	logPath := installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-capture-join-test"}

	_ = term.CapturePane(agent)

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux was never invoked: %v", err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "capture-pane -t hive-capture-join-test -p -J -S") {
		t.Fatalf("CapturePane sent %q, want capture-pane with -p -J -S", logged)
	}
}

func TestTmuxTerminalClearHistorySendsClearHistoryCommand(t *testing.T) {
	logPath := installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
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

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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type attachFakeTmux struct {
	logPath     string
	clientsPath string
	exitPath    string
}

// installAttachFakeTmux puts a fake tmux first on PATH whose list-clients
// reads its output and exit code from per-test state files, and which appends
// every invocation to logPath. The state deliberately avoids process-global
// env vars (#7145): package-level background tmux probes can inherit PATH
// while this fake is installed, and env-driven output makes the fake depend
// on whichever subtest configuration is live at process start.
func installAttachFakeTmux(t *testing.T) attachFakeTmux {
	t.Helper()
	dir := t.TempDir()
	fake := attachFakeTmux{
		logPath:     filepath.Join(dir, "tmux.log"),
		clientsPath: filepath.Join(dir, "clients.out"),
		exitPath:    filepath.Join(dir, "clients.exit"),
	}
	fake.setClients(t, "", "0")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %s || :
case "$*" in
  *list-clients*)
    cat %s
    code=$(cat %s)
    exit "${code:-0}"
    ;;
esac
exit 0
`, shellQuote(fake.logPath), shellQuote(fake.clientsPath), shellQuote(fake.exitPath))
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fake
}

func (f attachFakeTmux) setClients(t *testing.T, output, exit string) {
	t.Helper()
	if err := os.WriteFile(f.clientsPath, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.exitPath, []byte(exit), 0o644); err != nil {
		t.Fatal(err)
	}
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
	fake := installAttachFakeTmux(t)
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
			fake.setClients(t, tc.output, tc.exit)
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
	fake := installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}

	_ = term.SessionAttached(&AgentProcess{Name: "quality", tmuxSession: "hive-activity-probe"})

	raw, err := os.ReadFile(fake.logPath)
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
	m := NewManager(nil, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-seam"}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		t.Fatal("wrapper should report detached when tmux says 0 clients")
	}
}

func TestTmuxTerminalSessionAttachedIgnoresStaleEnvConfiguration(t *testing.T) {
	// This pins the issue #7145 root cause: the fake tmux used to read its
	// result from HIVE_FAKE_TMUX_* env vars, so unrelated package goroutines
	// that exec tmux while PATH points at this fake could observe whichever
	// subtest env happened to be live. A clean file-backed "no clients" with
	// exit 0 must stay detached even if stale env asks to fail open.
	installAttachFakeTmux(t)
	t.Setenv("HIVE_FAKE_TMUX_CLIENTS", "not-a-number\n")
	t.Setenv("HIVE_FAKE_TMUX_CLIENTS_EXIT", "1")

	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-attach-env-stale"}
	if term.SessionAttached(agent) {
		t.Fatal("SessionAttached should ignore stale env when fake state says no attached clients")
	}
}

func TestTmuxTerminalCapturePaneJoinsWrappedLinesCommand(t *testing.T) {
	fake := installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-capture-join-test"}

	_ = term.CapturePane(agent)

	raw, err := os.ReadFile(fake.logPath)
	if err != nil {
		t.Fatalf("fake tmux was never invoked: %v", err)
	}
	logged := string(raw)
	if !strings.Contains(logged, "capture-pane -t hive-capture-join-test -p -J -S") {
		t.Fatalf("CapturePane sent %q, want capture-pane with -p -J -S", logged)
	}
}

func TestTmuxTerminalClearHistorySendsClearHistoryCommand(t *testing.T) {
	fake := installAttachFakeTmux(t)
	term := tmuxTerminal{manager: NewManager(nil, discardLogger(), ProjectContext{})}
	agent := &AgentProcess{Name: "quality", tmuxSession: "hive-clear-test"}

	term.ClearHistory(agent)

	raw, err := os.ReadFile(fake.logPath)
	if err != nil {
		t.Fatalf("fake tmux was never invoked: %v", err)
	}
	if !strings.Contains(string(raw), "clear-history -t hive-clear-test") {
		t.Fatalf("ClearHistory sent %q, want clear-history -t hive-clear-test", string(raw))
	}
}

package agent

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/crux/config"
)

// A kick is typed into the pane as 400-rune chunks via `tmux send-keys -l`.
// Before the "--" end-of-options marker was added, a chunk that happened to
// start with "-" (a chunk boundary landing just before the hyphen in
// "onboard-ai-platform", "hold-gated", "[ONB-2651]", or a markdown bullet) was
// parsed by tmux as a flag, rejected with "unknown flag", and silently
// dropped — one 400-character slice of the kick never reached the agent.
// This types such a chunk through the production SendLiteral and asserts it
// lands in the pane verbatim.
func TestTmuxTerminal_SendLiteral_TypesTextStartingWithDash(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	const name = "sendlit-dash"
	session := "hive-" + name
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{name: {Backend: "claude"}}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, name)
	m.mu.Lock()
	agent := m.agents[name]
	agent.tmuxSession = session
	m.mu.Unlock()

	// Exactly the shape of the chunk that was lost in production: the slice
	// after "onboard-ai" began with "-platform#1488 …".
	const chunk = "-platform#1488 [ONB-2651]: Cover routes/invitations.py"
	tmuxTerminal{manager: m}.SendLiteral(agent, chunk)

	// No Enter is sent: the text sits on the shell's input line, which is
	// enough for capture-pane to see it and keeps the shell from executing it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		pane := tmuxTerminal{manager: m}.CaptureVisiblePane(agent)
		if strings.Contains(pane, chunk) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chunk starting with %q never reached the pane; pane was:\n%s", chunk[:1], pane)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exercise the production terminal adapter without a live tmux server.
func TestTmuxTerminalReturnsWriteErrors(t *testing.T) {
	dir := filepath.Dir(writeFakeTmux(t, "#!/bin/sh\nexit 17\n"))
	t.Setenv("PATH", dir)
	m, a, _ := kickLogTestManager(t, "")
	terminal := tmuxTerminal{manager: m}
	for _, err := range []error{terminal.SendLiteral(a, "task"), terminal.SendKeys(a, "Enter")} {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
			t.Fatalf("write error = %v, want tmux exit status 17", err)
		}
	}
}

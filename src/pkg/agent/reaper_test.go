package agent

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestContainsCLIMarker(t *testing.T) {
	cases := []struct {
		cmdline string
		want    bool
	}{
		{"claude --model claude-opus-4.7 --dangerously-skip-permissions", true},
		{"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js", true},
		{"copilot --model gpt-5 --allow-all", true},
		{"gemini --model gemini-2.5-pro", true},
		{"goose run -s", true},
		{"bob", true},
		{"/bin/bash -l", false},
		{"tmux new-session -d -s hive-scanner", false},
		{"", false},
	}
	for _, c := range cases {
		if got := containsCLIMarker(c.cmdline); got != c.want {
			t.Errorf("containsCLIMarker(%q) = %v, want %v", c.cmdline, got, c.want)
		}
	}
}

func TestEnvironHasMarker(t *testing.T) {
	// /proc environ is NUL-separated KEY=VALUE pairs.
	environ := "PATH=/usr/bin\x00HIVE_AGENT=scanner\x00HOME=/data/home\x00"
	if !environHasMarker(environ, "HIVE_AGENT=scanner") {
		t.Errorf("expected match for exact agent")
	}
	// Prefix collision guard: scanner must not match scanner-2 and vice versa.
	if environHasMarker(environ, "HIVE_AGENT=scanner-2") {
		t.Errorf("scanner-2 must not match scanner's environ")
	}
	environ2 := "HIVE_AGENT=scanner-2\x00"
	if environHasMarker(environ2, "HIVE_AGENT=scanner") {
		t.Errorf("scanner must not match scanner-2's environ (prefix collision)")
	}
	if !environHasMarker(environ2, "HIVE_AGENT=scanner-2") {
		t.Errorf("expected match for scanner-2")
	}
	if environHasMarker("", "HIVE_AGENT=scanner") {
		t.Errorf("empty environ must not match")
	}
}

// TestReapAgentCLI_KillsMarkedProcess spawns a real process carrying the
// HIVE_AGENT env marker and a cmdline that looks like a CLI, then verifies
// reapAgentCLI kills it — and leaves an unrelated agent's process alone. This
// exercises the /proc scan, so it only runs on Linux.
func TestReapAgentCLI_KillsMarkedProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("reapAgentCLI reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}

	m := NewManager(map[string]config.AgentConfig{
		"scanner":   {Backend: "claude"},
		"scanner-2": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Long-lived process whose cmdline contains "goose" (a CLI marker) tagged as
	// belonging to "scanner". Using goose avoids needing a real claude binary.
	target := exec.Command("goose", "3600")
	target.Path = "/bin/sleep" // exec the real sleep; argv[0] still carries "goose"
	target.Args = []string{"goose", "3600"}
	target.Env = []string{"HIVE_AGENT=scanner", "PATH=/usr/bin:/bin"}
	if err := target.Start(); err != nil {
		t.Skipf("could not start target process: %v", err)
	}
	defer func() { _ = target.Process.Kill() }()

	// Decoy belonging to a different agent must NOT be reaped.
	decoy := exec.Command("goose", "3600")
	decoy.Path = "/bin/sleep"
	decoy.Args = []string{"goose", "3600"}
	decoy.Env = []string{"HIVE_AGENT=scanner-2", "PATH=/usr/bin:/bin"}
	if err := decoy.Start(); err != nil {
		t.Skipf("could not start decoy process: %v", err)
	}
	defer func() { _ = decoy.Process.Kill() }()

	// Give /proc time to expose the new PIDs' cmdline/environ.
	time.Sleep(200 * time.Millisecond)

	agent := m.agents["scanner"]
	reaped := m.reapAgentCLI(agent)
	if reaped < 1 {
		t.Fatalf("reapAgentCLI reaped %d, want >= 1", reaped)
	}

	// Wait reaps the SIGKILL'd child (otherwise it lingers as a zombie that
	// kill(pid,0) still reports as alive). Wait returning is itself proof the
	// process was terminated by the reaper.
	waitErr := target.Wait()
	if waitErr == nil {
		t.Errorf("target exited cleanly; expected it to be killed by the reaper")
	}
	if ws, ok := exitSignal(waitErr); ok && ws != syscall.SIGKILL {
		t.Errorf("target killed by %v, want SIGKILL", ws)
	}

	// The decoy (different agent) must still be alive — the reaper must match
	// the exact agent, never another agent sharing the dev UID.
	if !processAlive(decoy.Process.Pid) {
		t.Errorf("decoy pid %d (scanner-2) was killed; reaper matched wrong agent", decoy.Process.Pid)
	}
}

// processAlive reports whether a PID exists (signal 0 probes without delivering).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// exitSignal extracts the terminating signal from an *exec.Cmd Wait error.
func exitSignal(err error) (syscall.Signal, bool) {
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return 0, false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

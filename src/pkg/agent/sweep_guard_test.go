package agent

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Regression for #9416: an agent running hive's own test suite inside its tmux
// pane must never SIGKILL its own process tree via the real-/proc sweeps.

func TestSweepGuard_RefusesRealProcInsideTests(t *testing.T) {
	if got := sweepGuardReason(realProcRoot); got == "" {
		t.Fatal("sweepGuardReason(/proc) = \"\" inside a test binary; want a refusal reason")
	}
	if got := sweepGuardReason(t.TempDir()); got != "" {
		t.Fatalf("sweepGuardReason(fake root) = %q; fake proc trees must stay sweepable", got)
	}
}

func TestSweepGuard_KillHelpersNoOpOnRealProc(t *testing.T) {
	victim := exec.Command("/bin/sleep", "3600")
	if err := victim.Start(); err != nil {
		t.Skipf("could not start victim process: %v", err)
	}
	defer func() { _ = victim.Process.Kill() }()

	// procRoot is the real /proc here; both sweeps must return 0 without
	// touching anything, even for our own (valid, >= floor) UID.
	uid := os.Getuid()
	if uid < minAgentUID {
		uid = minAgentUID
	}
	if killed := killAgentProcesses(uid, discardLogger()); killed != 0 {
		t.Fatalf("killAgentProcesses on real /proc inside a test killed %d; want 0", killed)
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if reaped := m.reapAgentCLI(agent); reaped != 0 {
		t.Fatalf("reapAgentCLI on real /proc inside a test reaped %d; want 0", reaped)
	}
	// Signal 0 probes liveness without delivering anything.
	if err := victim.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("victim is gone (%v); sweep guard did not hold", err)
	}
}

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// startSleeper launches a real child and registers its cleanup. Real processes
// matter here: with a fake pid syscall.Kill fails, the counter never moves, and
// a test asserting "0 reaped" passes whether the predicate matched or not.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
	return c
}

// assertKilled confirms the sweep actually delivered SIGKILL.
//
// A signal-0 liveness probe is NOT sufficient here and reports the opposite of
// the truth: SIGKILL leaves a child of this test process as a ZOMBIE until it
// is waited on, and signal 0 succeeds against a zombie. Waiting and reading the
// exit status is what distinguishes "killed" from "still running".
func assertKilled(t *testing.T, name string, c *exec.Cmd) {
	t.Helper()
	_ = c.Wait()
	ws, ok := c.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("%s: cannot read wait status", name)
	}
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("%s survived the sweep (exit state %v)", name, c.ProcessState)
	}
}

// assertRunning confirms a process was left alone. It has not been signalled,
// so it cannot be a zombie and signal 0 is a truthful probe.
func assertRunning(t *testing.T, name string, c *exec.Cmd) {
	t.Helper()
	if err := syscall.Kill(c.Process.Pid, 0); err != nil {
		t.Errorf("%s was killed by the marker sweep: %v", name, err)
	}
}

func reaperFor(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	return m, agent
}

// TestReapsAgentSpawnedChild is the #4924 regression.
//
// A poll loop the agent spawned carries HIVE_AGENT by inheritance but is not
// named after a backend binary. Under the old cmdline allowlist it survived
// every task; on the workstation path nothing else collected it, because
// killAgentProcesses is gated on UID isolation being on.
func TestReapsAgentSpawnedChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)

	child := startSleeper(t)
	writeProcEntry(t, root, child.Process.Pid,
		"/bin/bash\x00-c\x00until gh pr checks 4914; do sleep 60; done",
		"PATH=/bin\x00HIVE_AGENT=scanner\x00", os.Getuid())

	m, agent := reaperFor(t)
	if killed := m.reapAgentCLI(agent); killed != 1 {
		t.Fatalf("reaped %d, want 1 — an agent-spawned child must not outlive its agent", killed)
	}
	assertKilled(t, "spawned poll loop", child)
}

// TestReapSkipsTmux: tmux owns every pane for the agent, and on a shared socket
// for other agents too. Even carrying the marker it must never be signalled.
func TestReapSkipsTmux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)

	tmux := startSleeper(t)
	writeProcEntry(t, root, tmux.Process.Pid, "tmux\x00new-session\x00-d\x00-s\x00hive-scanner",
		"HIVE_AGENT=scanner\x00", os.Getuid())

	m, agent := reaperFor(t)
	if killed := m.reapAgentCLI(agent); killed != 0 {
		t.Fatalf("reaped %d, want 0 — killing tmux takes every pane down with it", killed)
	}
	assertRunning(t, "tmux", tmux)
}

// TestReapSkipsOtherAgent: the marker is compared as a whole NUL-separated
// entry, so one agent's sweep can never reach another's processes — including
// the prefix case, where "scanner" must not match "scanner-2".
func TestReapSkipsOtherAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)

	other := startSleeper(t)
	prefix := startSleeper(t)
	writeProcEntry(t, root, other.Process.Pid, "sleep\x0030", "HIVE_AGENT=quality\x00", os.Getuid())
	writeProcEntry(t, root, prefix.Process.Pid, "sleep\x0030", "HIVE_AGENT=scanner-2\x00", os.Getuid())

	m, agent := reaperFor(t)
	if killed := m.reapAgentCLI(agent); killed != 0 {
		t.Fatalf("reaped %d, want 0", killed)
	}
	assertRunning(t, "another agent's process", other)
	assertRunning(t, "scanner-2 (prefix collision)", prefix)
}

// TestReapSkipPID asserts the guard DIRECTLY rather than through the reaped
// count.
//
// Going through the count would be vacuous: an unprivileged test process cannot
// kill real PID 1, so syscall.Kill fails, the counter stays 0, and the test
// passes whether or not the guard exists. Verified by mutation — deleting the
// guard left a count-based test green.
func TestReapSkipPID(t *testing.T) {
	const self = 4242
	cases := []struct {
		name string
		pid  int
		want bool
	}{
		{name: "init is never signalled", pid: 1, want: true},
		{name: "hive never signals itself", pid: self, want: true},
		{name: "an ordinary agent pid is fair game", pid: 9001, want: false},
	}
	for _, c := range cases {
		if got := reapSkipPID(c.pid, self); got != c.want {
			t.Errorf("%s: reapSkipPID(%d, %d) = %v, want %v", c.name, c.pid, self, got, c.want)
		}
	}
}

// TestReapNeverSignalsPIDOne records every pid the sweep tries to kill, rather
// than counting the ones it succeeded in killing.
//
// That distinction is the whole test. An unprivileged process cannot kill real
// PID 1, so syscall.Kill fails and the reaped count stays 0 whether the guard
// exists or not — verified by mutation: removing the guard from the loop left a
// count-based version of this test green. Recording the ATTEMPT is what makes
// "hive never signals init" an assertion instead of an accident.
func TestReapNeverSignalsPIDOne(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)
	// init, wearing the agent's marker — the worst case the guard exists for.
	writeProcEntry(t, root, 1, "/sbin/init", "HIVE_AGENT=scanner\x00", 0)
	// A normal agent process, so the sweep is not trivially a no-op.
	writeProcEntry(t, root, 9001, "sleep\x0060", "HIVE_AGENT=scanner\x00", os.Getuid())

	var attempted []int
	orig := reapKill
	reapKill = func(pid int, sig syscall.Signal) error {
		attempted = append(attempted, pid)
		return nil
	}
	t.Cleanup(func() { reapKill = orig })

	m, agent := reaperFor(t)
	m.reapAgentCLI(agent)

	for _, pid := range attempted {
		if pid == 1 {
			t.Fatal("the sweep signalled PID 1 — that takes down init")
		}
	}
	if len(attempted) == 0 {
		t.Fatal("the sweep signalled nothing; the fixture is not exercising the kill path")
	}
}

// TestReapSkipsUnmarkedProcess: the agent's own pane bash is created before the
// session env is set, so it never carries the marker and must be left alone —
// the ordering RelaunchBobAgentsAwaitingKey depends on.
func TestReapSkipsUnmarkedProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)

	shell := startSleeper(t)
	writeProcEntry(t, root, shell.Process.Pid, "/bin/bash", "PATH=/bin\x00TERM=xterm\x00", os.Getuid())

	m, agent := reaperFor(t)
	if killed := m.reapAgentCLI(agent); killed != 0 {
		t.Fatalf("reaped %d, want 0", killed)
	}
	assertRunning(t, "the pane's unmarked bash", shell)
}

// TestReapCountsCLIAndChildrenTogether: the mixed case an operator actually
// has — a CLI plus two things it spawned, all reaped in one pass.
func TestReapCountsCLIAndChildrenTogether(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the reaper reads /proc; Linux only (GOOS=%s)", runtime.GOOS)
	}
	root := t.TempDir()
	withFakeProc(t, root)

	cli := startSleeper(t)
	poller := startSleeper(t)
	server := startSleeper(t)
	writeProcEntry(t, root, cli.Process.Pid, "node\x00claude\x00--model\x00opus", "HIVE_AGENT=scanner\x00", os.Getuid())
	writeProcEntry(t, root, poller.Process.Pid, "sleep\x0060", "HIVE_AGENT=scanner\x00", os.Getuid())
	writeProcEntry(t, root, server.Process.Pid, "node\x00server.js", "HIVE_AGENT=scanner\x00", os.Getuid())
	os.MkdirAll(filepath.Join(root, "not-a-pid"), 0o755)

	m, agent := reaperFor(t)
	if killed := m.reapAgentCLI(agent); killed != 3 {
		t.Fatalf("reaped %d, want 3 (CLI + poller + server)", killed)
	}
	assertKilled(t, "cli", cli)
	assertKilled(t, "poller", poller)
	assertKilled(t, "server", server)
}

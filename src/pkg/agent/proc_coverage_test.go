package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// withFakeProc redirects procRoot to a temp dir for the duration of a test.
func withFakeProc(t *testing.T, dir string) {
	t.Helper()
	orig := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = orig })
}

// writeProcEntry creates a fake /proc/<pid>/ dir with cmdline, environ, and
// status files (NUL-separated where the kernel uses NUL).
func writeProcEntry(t *testing.T, root string, pid int, cmdline, environ string, uid int) {
	t.Helper()
	d := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644)
	os.WriteFile(filepath.Join(d, "environ"), []byte(environ), 0o644)
	status := fmt.Sprintf("Name:\ttest\nUid:\t%d\t%d\t%d\t%d\n", uid, uid, uid, uid)
	os.WriteFile(filepath.Join(d, "status"), []byte(status), 0o644)
}

// TestReapAgentCLI_FakeProc: the reaper scans the fake /proc, matches a process
// by CLI marker + HIVE_AGENT env, and attempts to kill it. We use a real
// short-lived child process so the syscall.Kill actually succeeds (killed++).
func TestReapAgentCLI_FakeProc(t *testing.T) {
	root := t.TempDir()
	withFakeProc(t, root)

	// A real child to be reaped.
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := child.Process.Pid
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()

	// Matching entry: cmdline names "claude", environ has HIVE_AGENT=scanner.
	writeProcEntry(t, root, pid, "node\x00claude\x00--model\x00opus",
		"PATH=/bin\x00HIVE_AGENT=scanner\x00", os.Getuid())
	// A non-matching entry (different agent) that must be skipped.
	writeProcEntry(t, root, pid+100000, "node\x00claude",
		"HIVE_AGENT=other\x00", os.Getuid())
	// #4924: a non-CLI process carrying THIS agent's marker — a child the agent
	// spawned. It used to be skipped by the cmdline allowlist; it must now be
	// reaped. A REAL child, because a fake pid makes syscall.Kill fail and the
	// counter would not move whether the predicate matched or not — the test
	// would pass either way and prove nothing.
	child2 := exec.Command("sleep", "30")
	if err := child2.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	defer func() { _ = child2.Process.Kill(); _, _ = child2.Process.Wait() }()
	writeProcEntry(t, root, child2.Process.Pid, "/bin/bash\x00-c\x00until gh pr checks; do sleep 60; done",
		"HIVE_AGENT=scanner\x00", os.Getuid())
	// A non-numeric dir entry (ignored).
	os.MkdirAll(filepath.Join(root, "not-a-pid"), 0o755)

	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	killed := m.reapAgentCLI(agent)
	if killed != 2 {
		t.Errorf("expected 2 reaped processes (the CLI and its spawned child), got %d", killed)
	}
}

func TestReapAgentCLI_FakeProcNoMatch(t *testing.T) {
	root := t.TempDir()
	withFakeProc(t, root)
	// Only entries for a DIFFERENT agent. Since #4924 the cmdline no longer
	// narrows anything, so the agent marker is the whole test: "/bin/bash" with
	// HIVE_AGENT=scanner would now be reaped, and this case has to key off the
	// name mismatch instead to still mean something.
	writeProcEntry(t, root, 424242, "/bin/bash", "HIVE_AGENT=other\x00", os.Getuid())
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if killed := m.reapAgentCLI(agent); killed != 0 {
		t.Errorf("expected 0 reaped, got %d", killed)
	}
}

func TestReapAgentCLI_ReadDirError(t *testing.T) {
	withFakeProc(t, filepath.Join(t.TempDir(), "does-not-exist"))
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	if killed := m.reapAgentCLI(agent); killed != 0 {
		t.Errorf("readdir error -> 0, got %d", killed)
	}
}

// TestKillAgentProcesses_FakeProc: kills a real child whose fake /proc/<pid>/
// status reports the target UID.
func TestKillAgentProcesses_FakeProc(t *testing.T) {
	root := t.TempDir()
	withFakeProc(t, root)

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start child: %v", err)
	}
	pid := child.Process.Pid
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()

	const targetUID = 4242
	writeProcEntry(t, root, pid, "sleep\x0030", "", targetUID)
	// Non-matching UID entry -> skipped.
	writeProcEntry(t, root, pid+100000, "other", "", 5555)
	// Non-numeric dir -> skipped.
	os.MkdirAll(filepath.Join(root, "kthreadd"), 0o755)

	killed := killAgentProcesses(targetUID, discardLogger())
	if killed != 1 {
		t.Errorf("expected 1 killed, got %d", killed)
	}
}

func TestKillAgentProcesses_RefusesUnsafeUID(t *testing.T) {
	root := t.TempDir()
	withFakeProc(t, root)

	writeProcEntry(t, root, os.Getpid(), "hive\x00test", "", 0)
	if killed := killAgentProcesses(0, discardLogger()); killed != 0 {
		t.Errorf("uid 0 cleanup should be refused, got %d kills", killed)
	}
	if killed := killAgentProcesses(-1, discardLogger()); killed != 0 {
		t.Errorf("negative uid cleanup should be refused, got %d kills", killed)
	}
}

func TestKillAgentProcesses_SkipsCurrentProcess(t *testing.T) {
	root := t.TempDir()
	withFakeProc(t, root)

	const targetUID = 4242
	writeProcEntry(t, root, os.Getpid(), "hive\x00test", "", targetUID)
	if killed := killAgentProcesses(targetUID, discardLogger()); killed != 0 {
		t.Errorf("current process should be skipped, got %d kills", killed)
	}
}

func TestKillAgentProcesses_ReadDirError(t *testing.T) {
	withFakeProc(t, filepath.Join(t.TempDir(), "missing"))
	if killed := killAgentProcesses(1, discardLogger()); killed != 0 {
		t.Errorf("readdir error -> 0, got %d", killed)
	}
}

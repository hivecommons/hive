package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// writeFakeProcEntry creates <root>/<pid>/status with the given owner UID,
// mimicking the two fields killAgentProcesses reads from a real /proc.
func writeFakeProcEntry(t *testing.T, root string, pid, ownerUID int) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fake proc entry: %v", err)
	}
	// Real /proc status has many lines; killAgentProcesses only parses "Uid:".
	status := fmt.Sprintf("Name:\tsleep\nPid:\t%d\nUid:\t%d\t%d\t%d\t%d\n", pid, ownerUID, ownerUID, ownerUID, ownerUID)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatalf("write fake status: %v", err)
	}
}

// withProcRoot points the package-level procRoot at a temp dir for the duration
// of the test, restoring it afterward.
func withProcRoot(t *testing.T, root string) {
	t.Helper()
	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })
}

// TestKillAgentProcesses_FloorGuard is the regression for kubestellar/hive#2743
// (candidate #1): killAgentProcesses(0) and any uid < minAgentUID must kill
// NOTHING even when a fake /proc contains a process owned by that UID. Before
// the fix, running as root this SIGKILLed every root-owned process (hive, PID 1,
// the tmux server). The floor guard makes it a guaranteed no-op.
func TestKillAgentProcesses_FloorGuard(t *testing.T) {
	// Spawn a real, long-lived child we can safely try to "kill". If the guard
	// leaks, this process would receive SIGKILL; if it holds, it stays alive.
	victim := exec.Command("/bin/sleep", "3600")
	if err := victim.Start(); err != nil {
		t.Skipf("could not start victim process: %v", err)
	}
	defer func() { _ = victim.Process.Kill() }()
	victimPID := victim.Process.Pid

	root := t.TempDir()
	withProcRoot(t, root)

	// Sub-floor UIDs the guard must refuse — including the exploitable root (0).
	subFloorUIDs := []int{0, 1, proxyUserUID, minAgentUID - 1}
	for _, uid := range subFloorUIDs {
		// Make the fake proc entry OWNED by the queried uid, so only the floor
		// guard — not an ownership mismatch — can prevent the kill.
		writeFakeProcEntry(t, root, victimPID, uid)

		killed := killAgentProcesses(uid, discardLogger())
		if killed != 0 {
			t.Errorf("killAgentProcesses(%d) returned %d, want 0 (floor guard must block)", uid, killed)
		}
		if !processAlive(victimPID) {
			t.Fatalf("killAgentProcesses(%d) SIGKILLed a uid<%d process (pid %d) — floor guard leaked",
				uid, minAgentUID, victimPID)
		}
	}
}

// TestKillAgentProcesses_SelfSkip ensures the sweep never SIGKILLs the hive
// process itself, even when our own PID appears in /proc owned by an agent UID.
func TestKillAgentProcesses_SelfSkip(t *testing.T) {
	root := t.TempDir()
	withProcRoot(t, root)

	// Our own PID, reported as owned by a legitimate agent UID (>= minAgentUID).
	writeFakeProcEntry(t, root, os.Getpid(), minAgentUID)

	killed := killAgentProcesses(minAgentUID, discardLogger())
	if killed != 0 {
		t.Errorf("killAgentProcesses matched and killed %d process(es); must self-skip own PID", killed)
	}
}

// TestKillAgentProcesses_KillsAgentUID confirms the guard does not over-block:
// a real per-agent UID (>= minAgentUID) still sweeps a matching process. Uses a
// spawned child and a fake /proc entry reporting that child's PID.
func TestKillAgentProcesses_KillsAgentUID(t *testing.T) {
	victim := exec.Command("/bin/sleep", "3600")
	if err := victim.Start(); err != nil {
		t.Skipf("could not start victim process: %v", err)
	}
	defer func() { _ = victim.Process.Kill() }()
	victimPID := victim.Process.Pid

	root := t.TempDir()
	withProcRoot(t, root)
	// Report the child as owned by a valid agent UID above the floor.
	writeFakeProcEntry(t, root, victimPID, minAgentUID+5)

	killed := killAgentProcesses(minAgentUID+5, discardLogger())
	if killed != 1 {
		t.Fatalf("killAgentProcesses returned %d, want 1 (should kill the matching agent process)", killed)
	}

	// Confirm the SIGKILL landed. Wait reaps the zombie; a non-nil error is proof.
	if waitErr := victim.Wait(); waitErr == nil {
		t.Errorf("victim exited cleanly; expected SIGKILL from killAgentProcesses")
	} else if ws, ok := exitSignal(waitErr); ok && ws != syscall.SIGKILL {
		t.Errorf("victim killed by %v, want SIGKILL", ws)
	}
}

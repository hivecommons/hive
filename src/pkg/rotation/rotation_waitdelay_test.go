package rotation

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"
)

// Regression: a probe child that forks a GRANDCHILD inheriting its output pipe
// used to hang exec.Cmd.Wait forever — killing the direct child does not close
// the pipe's write end held by the grandchild, so the copier goroutine exec
// creates for an io.Writer Stdout/Stderr (CodexProber sets Stderr = io.Discard;
// runCLI's CombinedOutput wires both) never sees EOF and Wait blocks on it.
// Observed live on weavster: the watchdog's codex auth probe
// (CodexProber.Probe's deferred Kill+Wait) wedged the main governor goroutine
// for 2.5 hours, stopping every eval and advisory-digest post until the hub
// flagged the digest stale.
//
// probeWaitDelay is the fix: exec.Cmd.WaitDelay force-closes the pipes after
// the delay and Wait returns instead of hanging. This test builds the same
// shape — sh forks a sleeping grandchild sharing the pipes, the direct child
// exits — and asserts Wait returns promptly. Without WaitDelay it blocks until
// the grandchild's sleep finishes (60s), far past the assertion's deadline
// (verified by running this exact shape without WaitDelay: it hangs).
func TestProbeWaitDelayUnblocksGrandchildPipeHang(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	// Grandchild inherits stdout/stderr and sleeps well past every bound
	// involved; the direct child exits immediately — the exact weavster shape.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 60 & exit 0")
	cmd.WaitDelay = probeWaitDelay
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// ErrWaitDelay (pipes force-closed) or nil are both fine — the point
		// is that Wait RETURNED.
		t.Logf("Wait returned: %v", err)
	case <-time.After(probeWaitDelay + probeTimeout):
		t.Fatal("cmd.Wait still blocked after WaitDelay — grandchild pipe hang regressed")
	}
}

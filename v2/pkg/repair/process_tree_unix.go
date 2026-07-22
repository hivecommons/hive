//go:build !windows

package repair

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func startRepairProcessTree(ctx context.Context, command *exec.Cmd) (func() error, error) {
	// A process group is best-effort lifecycle cleanup on Unix: a descendant
	// may deliberately create a new session. Integrity does not depend on this
	// boundary; Worker commits only the immutable, guarded candidate tree that
	// was sealed before validation, never mutable post-tool worktree bytes.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	pid := command.Process.Pid
	return func() error {
		waitDone := make(chan error, 1)
		go func() { waitDone <- command.Wait() }()
		var waitErr error
		var waitTimedOut bool
		select {
		case waitErr = <-waitDone:
			// The leader exited normally. Reap any descendants that retained
			// inherited handles before returning to output-drain cleanup.
		case <-ctx.Done():
			// Cancellation kills the exact process group before waiting; otherwise
			// a descendant retaining stdout/stderr could hold Cmd.Wait forever.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			select {
			case waitErr = <-waitDone:
			case <-time.After(5 * time.Second):
				waitErr = ctx.Err()
				waitTimedOut = true
			}
		}
		killErr := syscall.Kill(-pid, syscall.SIGKILL)
		var containmentErr error
		if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			containmentErr = killErr
		}
		if waitTimedOut {
			containmentErr = errors.Join(containmentErr, errors.New("repair command did not exit within the bounded post-termination wait"))
		}
		if containmentErr != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("repair command result %v; process-tree containment failed: %w", waitErr, containmentErr))
		}
		return waitErr
	}, nil
}

// A Unix process group is best-effort containment: descendants can detach
// into another session. Keep the conservative snapshot deadline on restart.
var repairProcessTreeCrashContainmentGuaranteed = func() bool {
	return false
}

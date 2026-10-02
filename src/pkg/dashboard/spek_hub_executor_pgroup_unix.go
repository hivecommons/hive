//go:build !windows

package dashboard

import (
	"os/exec"
	"syscall"
)

// spekHubConfigureProcessGroup puts the stage command in its own process
// group and makes context cancellation kill that whole group, so agent CLI
// grandchildren die with their `sh` parent.
func spekHubConfigureProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
			return c.Process.Kill()
		}
		return nil
	}
}

// spekHubKillProcessGroup SIGKILLs whatever remains of the stage's process
// group once the stage process itself has exited, so orphans cannot keep the
// worktree fence flock held.
func spekHubKillProcessGroup(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
}

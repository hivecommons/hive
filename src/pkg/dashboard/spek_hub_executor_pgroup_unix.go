//go:build !windows

package dashboard

import (
	"os/exec"
	"strconv"
	"syscall"
)

// spekHubConfigureProcessGroup puts the stage command in its own process
// group and makes context cancellation kill that whole group, so agent CLI
// grandchildren die with their `sh` parent. userSpec is the su-exec user the
// command runs as ("" for the hive uid).
func spekHubConfigureProcessGroup(c *exec.Cmd, userSpec string) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		if err := spekHubKillGroup(c.Process.Pid, userSpec); err != nil {
			return c.Process.Kill()
		}
		return nil
	}
}

// spekHubKillProcessGroup SIGKILLs whatever remains of the stage's process
// group once the stage process itself has exited, so orphans cannot keep the
// worktree fence flock held.
func spekHubKillProcessGroup(c *exec.Cmd, userSpec string) {
	if c.Process == nil {
		return
	}
	_ = spekHubKillGroup(c.Process.Pid, userSpec)
}

// spekHubKillGroup SIGKILLs process group pgid. The hive uid cannot signal a
// stage CLI running as the executor user, so that kill goes through su-exec.
func spekHubKillGroup(pgid int, userSpec string) error {
	if userSpec == "" {
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}
	return exec.Command("su-exec", userSpec, "sh", "-c", `kill -KILL -- "-$1"`, "sh", strconv.Itoa(pgid)).Run()
}

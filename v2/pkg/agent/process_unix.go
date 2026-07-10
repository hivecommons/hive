//go:build !windows

package agent

import "syscall"

func killProcessPID(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

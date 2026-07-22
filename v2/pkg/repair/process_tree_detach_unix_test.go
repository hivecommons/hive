//go:build !windows

package repair

import "syscall"

func detachRepairTestSession() error {
	_, err := syscall.Setsid()
	return err
}

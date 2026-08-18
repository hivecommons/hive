//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

func configureDetachedProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func tryLockDaemonLease(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return err == nil, err
}

func tryReadDaemonLease(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return err == nil, err
}

func unlockDaemonLease(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func processMatchesExecutable(pid int, expected string) bool {
	if runtime.GOOS != "linux" {
		return processIsAlive(pid)
	}
	actual, err := os.Readlink(filepath.Join("/proc", fmt.Sprint(pid), "exe"))
	if err != nil {
		return false
	}
	expectedResolved, err := filepath.EvalSymlinks(expected)
	return err == nil && actual == expectedResolved
}

func processIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func terminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

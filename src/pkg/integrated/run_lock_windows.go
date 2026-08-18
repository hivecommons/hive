//go:build windows

package integrated

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func tryLockProductionRunLease(file *os.File) (bool, error) {
	overlapped := &windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == windows.ERROR_LOCK_VIOLATION {
		return false, nil
	}
	return err == nil, err
}

func unlockProductionRunLease(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{OffsetHigh: 1})
}

func legacyProductionRunOwnerMatches(pid int, startedAt time.Time) bool {
	if pid <= 0 || startedAt.IsZero() {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if windows.GetExitCodeProcess(handle, &exitCode) != nil || exitCode != 259 {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return true // a live owner with unavailable start identity must fail closed
	}
	createdAt := time.Unix(0, created.Nanoseconds()).UTC()
	if createdAt.IsZero() || createdAt.After(time.Now().UTC().Add(time.Minute)) {
		return true
	}
	// A v1 owner necessarily existed before it wrote StartedAt. A process with
	// the same PID that began later is a reused PID, regardless of the binary's
	// filename (Hive may be renamed or launched through a packaged wrapper).
	return !createdAt.After(startedAt.Add(3 * time.Second))
}

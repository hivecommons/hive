//go:build linux

package posture

import (
	"os"
	"syscall"
)

// Check reads this process's /proc/self/status and root filesystem.
func Check() []string { return check("/proc/self/status", syscall.Statfs) }

// check is Check with the status path and statfs call injectable for tests.
func check(statusPath string, statfs func(string, *syscall.Statfs_t) error) []string {
	f, err := os.Open(statusPath)
	if err != nil {
		return []string{"cannot read /proc/self/status: " + err.Error()}
	}
	defer f.Close()
	st, err := ParseStatus(f)
	if err != nil {
		return []string{err.Error()}
	}
	var fs syscall.Statfs_t
	if err := statfs("/", &fs); err != nil {
		// A root filesystem that cannot be inspected is a gap, like an
		// unreadable status file, so strict mode fails closed (#19479).
		return append(Gaps(st, false, false), "cannot check whether the root filesystem is read-only: "+err.Error())
	}
	return Gaps(st, true, fs.Flags&1 == 0) // ST_RDONLY
}

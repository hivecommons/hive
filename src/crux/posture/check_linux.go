//go:build linux

package posture

import (
	"os"
	"syscall"
)

// Check reads this process's /proc/self/status and root filesystem.
func Check() []string {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return []string{"cannot read /proc/self/status: " + err.Error()}
	}
	defer f.Close()
	st, err := ParseStatus(f)
	if err != nil {
		return []string{err.Error()}
	}
	var fs syscall.Statfs_t
	writable := false
	if syscall.Statfs("/", &fs) == nil {
		writable = fs.Flags&1 == 0 // ST_RDONLY
	}
	return Gaps(st, true, writable)
}

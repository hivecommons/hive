//go:build linux

package agent

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openEntry pins path with an O_PATH descriptor that refuses to follow a
// symlink (CWE-59 / CWE-367). An O_PATH descriptor pins the entry without
// needing read permission, so the watcher can still repair entries whose
// owner read bits are missing — the very modes it exists to fix. The
// descriptor cannot be used with fchmod or fchown, so changes go through its
// /proc/self/fd link instead. O_PATH comes from x/sys/unix: the standard
// syscall package does not define it.
func openEntry(path string) (*os.File, error) {
	return os.OpenFile(path, unix.O_PATH|syscall.O_NOFOLLOW, 0)
}

func entryChmod(f *os.File, mode os.FileMode) error {
	return os.Chmod(entryProcPath(f), mode)
}

func entryChown(f *os.File, uid, gid int) error {
	return os.Chown(entryProcPath(f), uid, gid)
}

func entryProcPath(f *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", f.Fd())
}

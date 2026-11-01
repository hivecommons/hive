//go:build !linux

package agent

import (
	"os"
	"syscall"
)

// openEntry pins path with a read-only descriptor that refuses to follow a
// symlink. O_PATH and /proc/self/fd are Linux-only, so entries without owner
// read permission are skipped here, exactly as fixModeFile skips them.
func openEntry(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func entryChmod(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

func entryChown(f *os.File, uid, gid int) error {
	return f.Chown(uid, gid)
}

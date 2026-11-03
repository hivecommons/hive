//go:build !linux

package agent

import (
	"os"
	"syscall"
)

// openEntry pins path with a read-only descriptor that refuses to follow a
// symlink. O_PATH and /proc/self/fd are Linux-only, so entries without owner
// read permission are skipped here, exactly as fixModeFile skips them.
// O_NONBLOCK keeps a FIFO under a watched root from stalling the watcher in
// open(2); pinEntry discards non-regular entries afterwards.
func openEntry(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func entryChmod(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

func entryChown(f *os.File, uid, gid int) error {
	return f.Chown(uid, gid)
}

// Package privatedir verifies that a socket directory belongs to the current
// user alone.
package privatedir

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Check refuses a directory this user does not own outright: MkdirAll is
// silent about a directory that already existed, and on a shared temp dir
// another user could have created it first and could then swap the socket.
// The directory must be a real directory (not a symlink), owned by this uid,
// with no group or other permission bits. want is only quoted in the error.
func Check(dir string, want os.FileMode) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("is not a directory")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %04o is readable by others; want %04o", perm, want)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not this user (uid %d)", st.Uid, os.Getuid())
	}
	return nil
}

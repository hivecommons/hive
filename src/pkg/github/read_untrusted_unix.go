//go:build unix

package github

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// readUntrustedFile reads an agent-supplied drop-box file without following
// symlinks (O_NOFOLLOW fails the open with ELOOP) and without blocking on
// FIFOs (O_NONBLOCK makes the open return immediately; it is a no-op for
// regular files). The type and size checks run on the OPEN descriptor, so no
// rename between ReadDir and open can swap in something else. Returns the
// post-open FileInfo so callers can take ownership (fileOwnerUID) from the
// same inode the bytes came from.
func readUntrustedFile(path string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if pe, ok := err.(*os.PathError); ok && pe.Err == syscall.ELOOP {
			return nil, nil, dropBoxRejectedError("symbolic link")
		}
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("not a regular file (%s)", fi.Mode().Type()))
	}
	if fi.Size() > maxBytes {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("%d bytes exceeds cap of %d", fi.Size(), maxBytes))
	}
	// LimitReader caps the read even if the file grows after the fstat.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fi, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("grew past cap of %d bytes during read", maxBytes))
	}
	return data, fi, nil
}

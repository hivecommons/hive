//go:build !unix

package github

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// readUntrustedFile off unix (dev/Windows builds): O_NOFOLLOW/O_NONBLOCK are
// not portable, so approximate with an Lstat type check before the open and
// the same post-open fstat type/size checks. The production target is Linux,
// which uses read_untrusted_unix.go.
func readUntrustedFile(path string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, lfi, dropBoxRejectedError(fmt.Sprintf("not a regular file (%s)", lfi.Mode().Type()))
	}
	f, err := os.Open(path)
	if err != nil {
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
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fi, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("grew past cap of %d bytes during read", maxBytes))
	}
	return data, fi, nil
}

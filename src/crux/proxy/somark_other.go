//go:build !linux

package proxy

// setSockMark is a no-op on non-Linux platforms: SO_MARK is a Linux-only socket
// option. The proxy only runs inside the Linux container, but the package must
// still compile on the CI build matrix (and on developer darwin machines), so
// this stub keeps the mark-setting dialer portable.
func setSockMark(fd uintptr, mark int) error {
	return nil
}

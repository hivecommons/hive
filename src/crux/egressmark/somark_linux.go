//go:build linux

package egressmark

import "syscall"

// setSockMark sets SO_MARK on the raw socket so the kernel stamps every packet
// it emits with the fwmark the entrypoint's nat/OUTPUT RETURN rule matches.
func setSockMark(fd uintptr, mark int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
}

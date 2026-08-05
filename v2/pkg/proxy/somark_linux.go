//go:build linux

package proxy

import (
	"syscall"
)

// setSockMark sets SO_MARK on the raw socket file descriptor. The kernel then
// stamps every packet the socket emits with this fwmark, which the entrypoint's
// iptables nat/OUTPUT rule matches (-m mark --mark <mark> -j RETURN) to exempt
// the proxy's OWN upstream traffic from the :443 -> :18443 redirect, so it does
// not loop back into itself.
//
// Unlike `-m owner --uid-owner`, packet marks do NOT require the xt_owner kernel
// module, which is absent on OpenShift/OVN nodes. The mark exemption is the
// single self-exemption mechanism that works on both OKE and OpenShift.
func setSockMark(fd uintptr, mark int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark)
}

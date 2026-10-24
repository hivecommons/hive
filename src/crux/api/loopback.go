package api

import (
	"net/netip"
	"strings"
)

// IsLoopbackHost reports whether host is localhost or a loopback IP literal.
// The host must have no port or IPv6 brackets. Other names are refused without
// DNS resolution, so a name resolving to loopback today cannot later send a
// bearer token over the network in cleartext.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// Package netguard is the single address classifier for server-side fetchers
// that act on a URL a user or a remote party supplied. It answers one
// question: may the hive connect to this address? Anything that is not
// globally routable is refused: loopback, RFC 1918, CGNAT, link-local
// (including cloud metadata), unique-local, multicast, unspecified, reserved
// and documentation ranges, and IPv4 carried inside IPv4-mapped, NAT64 or
// 6to4 IPv6 addresses.
//
// It parses with net/netip rather than matching text, so spellings that a
// prefix list misses (bare ::1, fc00::/7, 100.64.0.0/10, a userinfo section
// ahead of the host, decimal or hex integer literals) are judged by the
// address they denote. Every uncertain case is refused: fail closed.
package netguard

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strings"
)

// AllowCGNATEnv lifts the carrier-grade NAT refusal (100.64.0.0/10) for
// HostBlocked only. Overlay networks such as a tailnet number their members
// out of that range, so an operator whose hubs and spokes reach each other
// there sets it. Only the exact string "true" turns it on; unset keeps the
// range refused. Every other range stays refused, and AddrBlocked (the
// proxy-grade, link-fetch classifier) is not affected.
const AllowCGNATEnv = "HIVE_FETCH_ALLOW_CGNAT"

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

func allowCGNAT() bool {
	return strings.TrimSpace(os.Getenv(AllowCGNATEnv)) == "true"
}

// fetchAddrBlocked is AddrBlocked plus the optional CGNAT allowance.
func fetchAddrBlocked(addr netip.Addr) bool {
	if allowCGNAT() && addr.IsValid() && cgnatPrefix.Contains(addr.WithZone("").Unmap()) {
		return false
	}
	return AddrBlocked(addr)
}

// blockedPrefixes are ranges a fetcher may never reach. The netip predicates
// in AddrBlocked cover most of them; the list also catches the ranges the
// predicates do not name (CGNAT, reserved, documentation, Teredo).
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "this network"
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // CGNAT (RFC 6598)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, cloud metadata
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, broadcast
	"::/128",          // unspecified
	"::1/128",         // loopback
	"100::/64",        // discard-only
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentation
	"fc00::/7",        // unique local, fd00:ec2::254 metadata
	"fe80::/10",       // link-local
	"fec0::/10",       // site-local (deprecated)
	"ff00::/8",        // multicast
)

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// nat64Prefix and sixToFour embed an IPv4 address that is checked too.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// Byte offsets of the embedded IPv4 address inside a 16-byte IPv6 address.
const (
	nat64V4Offset    = 12
	sixToFourV4Start = 2
	ipv4Len          = 4
)

// AddrBlocked reports whether addr must not be connected to. An invalid
// address is blocked.
func AddrBlocked(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		b := addr.As16()
		switch {
		case nat64Prefix.Contains(addr):
			if AddrBlocked(netip.AddrFrom4([ipv4Len]byte(b[nat64V4Offset : nat64V4Offset+ipv4Len]))) {
				return true
			}
		case sixToFour.Contains(addr):
			if AddrBlocked(netip.AddrFrom4([ipv4Len]byte(b[sixToFourV4Start : sixToFourV4Start+ipv4Len]))) {
				return true
			}
		}
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Resolver resolves a host name to address strings (the shape of
// net.Resolver.LookupHost). Fetchers keep their own test seam of this type.
type Resolver func(ctx context.Context, host string) ([]string, error)

// ExtractHost returns the lower-cased host of rawURL as it will be dialled:
// the scheme, any userinfo, the port, IPv6 brackets and zone, and a trailing
// dot are removed. The input may be a full URL, "host:port" or a bare host.
// The userinfo is removed because "http://example.com@127.0.0.1/" connects to
// 127.0.0.1, not example.com.
func ExtractHost(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+len("://"):]
	}
	if i := strings.IndexAny(s, "/?#\\"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end >= 0 {
			s = s[1:end]
		} else {
			s = s[1:]
		}
	} else if i := strings.LastIndex(s, ":"); i >= 0 && strings.Count(s, ":") == 1 {
		s = s[:i]
	}
	if i := strings.Index(s, "%"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimRight(s, "."))
}

// legacyIPv4Literal reports whether host is made only of dot-separated
// numeric labels (decimal, octal or 0x hex, as inet_aton accepts, so
// "2130706433", "0177.0.0.1", "0x7f.1" and "127.1"). netip rejects these, but
// a resolver or HTTP client may accept them and connect to the loopback or
// private address they denote. No real host name has an all-numeric final
// label, so such a host is refused outright.
func legacyIPv4Literal(host string) bool {
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return false
		}
		digits := label
		if strings.HasPrefix(label, "0x") {
			digits = label[2:]
			if digits == "" {
				return false
			}
			for _, c := range digits {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
					return false
				}
			}
			continue
		}
		for _, c := range digits {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// HostBlocked reports whether a fetcher must refuse rawURL. It is true for
// the name "localhost", for any literal address (in any spelling) that
// AddrBlocked refuses, and for a name that resolves to a blocked address.
// A failed or empty resolution, or an answer that is not an IP address, is
// blocked: when the check cannot be evaluated the request is refused.
func HostBlocked(ctx context.Context, rawURL string, resolve Resolver) bool {
	host := ExtractHost(rawURL)
	if host == "" || strings.HasPrefix(host, "localhost") {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return fetchAddrBlocked(addr)
	}
	if legacyIPv4Literal(host) {
		return true
	}
	addrs, err := resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return true
	}
	for _, a := range addrs {
		ip, perr := netip.ParseAddr(strings.TrimSpace(a))
		if perr != nil || fetchAddrBlocked(ip) {
			return true
		}
	}
	return false
}

// DefaultResolver resolves with the system resolver.
func DefaultResolver(ctx context.Context, host string) ([]string, error) {
	return (&net.Resolver{}).LookupHost(ctx, host)
}

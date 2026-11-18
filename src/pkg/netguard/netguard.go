// Package netguard is the one address classifier for server-side outbound
// fetches that act on a URL a user or a remote party influences. It replaces
// the string-prefix blocklist each isPrivateURL copy used to carry (#18991,
// part of the egress audit #18986).
//
// The prefix list matched addresses as text, so it missed every spelling that
// is the same host but a different string: carrier-grade NAT (100.64.0.0/10),
// IPv6 unique-local (fc00::/7) and link-local (fe80::/10), a bare "::1" as the
// resolver returns it (no brackets), IPv4-mapped, NAT64 and 6to4 embeddings of
// a private IPv4 address, multicast, and the IPv4 literal forms an HTTP client
// or the C resolver accepts (2130706433, 0x7f.1, 0177.0.0.1, 127.1). Here
// every address is parsed with net/netip and classified by what it is, not by
// how it is written. The classification follows the dial-time guard in
// src/crux/proxy/plain_forward_guard.go (a separate module), extended with the
// embedded-IPv4 checks of src/pkg/inbox/link_fetch.go.
package netguard

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
)

// Reasons name the class that bars an address. They are stable strings so a
// caller may show them to a user.
const (
	ReasonLoopback    = "loopback"
	ReasonLinkLocal   = "link-local"
	ReasonMulticast   = "multicast"
	ReasonPrivate     = "private"
	ReasonCGNAT       = "carrier-grade NAT"
	ReasonUnspecified = "unspecified"
	ReasonThisNetwork = "this-network"
	ReasonBroadcast   = "broadcast"
	ReasonLocalName   = "local host name"
	// ReasonSpecialPurpose names an IANA special-purpose range that is not
	// globally routable and has no more specific reason: documentation,
	// benchmarking, protocol assignments, Teredo, discard-only, the 6to4
	// relay anycast block and deprecated site-local space.
	ReasonSpecialPurpose = "special-purpose"
	// ReasonNumericHost names a host written as dot-separated numbers that
	// is not a valid IPv4 literal (for example 1.2.3.4.5).
	ReasonNumericHost = "malformed numeric host"
)

var (
	// cgnatPrefix is RFC 6598 100.64.0.0/10: neither RFC 1918 (so
	// netip.Addr.IsPrivate does not report it) nor publicly routable.
	cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
	// thisNetwork is RFC 1122 0.0.0.0/8; Linux connects 0.x.y.z to the
	// local host.
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")
	// reservedV4 is 240.0.0.0/4 (reserved, includes 255.255.255.255).
	reservedV4 = netip.MustParsePrefix("240.0.0.0/4")
	// nat64Prefix (RFC 6052) and sixToFour (RFC 3056) carry an IPv4
	// address that is classified in its own right.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")

	// specialPurposePrefixes is the table of IANA special-purpose ranges
	// that no public service lives in. It is the range set documented in
	// src/docs/security-model.md; keep the two in step. The ranges the
	// switch in DenyReason already names (loopback, private, link-local,
	// multicast, CGNAT, this-network, reserved) are not repeated here.
	specialPurposePrefixes = mustPrefixes(
		"192.0.0.0/24",    // IETF protocol assignments (RFC 6890)
		"192.0.2.0/24",    // TEST-NET-1 documentation (RFC 5737)
		"192.88.99.0/24",  // 6to4 relay anycast (RFC 7526, deprecated)
		"198.18.0.0/15",   // benchmarking (RFC 2544)
		"198.51.100.0/24", // TEST-NET-2 documentation (RFC 5737)
		"203.0.113.0/24",  // TEST-NET-3 documentation (RFC 5737)
		"100::/64",        // discard-only (RFC 6666)
		"2001::/32",       // Teredo (RFC 4380)
		"2001:db8::/32",   // documentation (RFC 3849)
		"fec0::/10",       // site-local, deprecated (RFC 3879)
	)
)

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// DenyReason names the class that bars addr, or "" when addr is globally
// routable and may be fetched. An invalid address is refused.
func DenyReason(addr netip.Addr) string {
	if !addr.IsValid() {
		return ReasonUnspecified
	}
	// ::ffff:127.0.0.1 is the same host as 127.0.0.1, and a zone does not
	// change the class of an address.
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		b := addr.As16()
		switch {
		case nat64Prefix.Contains(addr):
			if r := DenyReason(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})); r != "" {
				return r
			}
		case sixToFour.Contains(addr):
			if r := DenyReason(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})); r != "" {
				return r
			}
		}
	}
	switch {
	case addr.IsUnspecified():
		return ReasonUnspecified
	case addr.IsLoopback():
		return ReasonLoopback
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10: cloud instance metadata lives here.
		return ReasonLinkLocal
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		return ReasonMulticast
	case addr.IsPrivate():
		// RFC 1918 for IPv4, fc00::/7 (unique local) for IPv6.
		return ReasonPrivate
	case cgnatPrefix.Contains(addr):
		return ReasonCGNAT
	case thisNetwork.Contains(addr):
		return ReasonThisNetwork
	case reservedV4.Contains(addr):
		return ReasonBroadcast
	}
	for _, p := range specialPurposePrefixes {
		if p.Contains(addr) {
			return ReasonSpecialPurpose
		}
	}
	return ""
}

// Denied reports whether addr is barred.
func Denied(addr netip.Addr) bool { return DenyReason(addr) != "" }

// ParseHostLiteral parses host as an IP literal in any spelling a client or
// resolver may accept: dotted quad, the inet_aton short, decimal, octal and
// hex forms (127.1, 2130706433, 0177.0.0.1, 0x7f.1), bracketed IPv6 with or
// without a zone (%25eth0 or %eth0), and a trailing dot. ok is false when
// host is a name rather than a literal.
func ParseHostLiteral(host string) (addr netip.Addr, ok bool) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if i := strings.Index(h, "%25"); i >= 0 {
		h = h[:i] + "%" + h[i+3:]
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.WithZone(""), true
	}
	return parseLooseIPv4(h)
}

// parseLooseIPv4 implements inet_aton: one to four parts separated by dots,
// each decimal, octal (leading 0) or hex (leading 0x); the last part fills
// the remaining bytes.
func parseLooseIPv4(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := parseLoosePart(p)
		if !ok {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var out [4]byte
	for i := 0; i < len(vals)-1; i++ {
		if vals[i] > 0xff {
			return netip.Addr{}, false
		}
		out[i] = byte(vals[i])
	}
	last := vals[len(vals)-1]
	rem := 4 - (len(vals) - 1)
	if last >= 1<<(8*uint(rem)) {
		return netip.Addr{}, false
	}
	for i := 3; i >= len(vals)-1; i-- {
		out[i] = byte(last)
		last >>= 8
	}
	return netip.AddrFrom4(out), true
}

func parseLoosePart(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := 10
	digits := p
	switch {
	case strings.HasPrefix(p, "0x"):
		base, digits = 16, p[2:]
		if digits == "" {
			// "0x" alone is zero to inet_aton.
			return 0, true
		}
	case len(p) > 1 && p[0] == '0':
		base, digits = 8, p[1:]
	}
	for _, c := range digits {
		if c == '+' || c == '-' || c == '_' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(digits, base, 32)
	if err != nil {
		return 0, false
	}
	return v, true
}

// URLHost returns the host a client would connect to for rawURL, lowercased,
// without scheme, userinfo, port, path, brackets or a trailing dot. It accepts
// a URL without a scheme ("example.com:443/x") and a bare IPv6 address.
func URLHost(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else {
		s = strings.TrimPrefix(s, "//")
	}
	if i := strings.IndexAny(s, "/?#\\"); i >= 0 {
		s = s[:i]
	}
	// The host follows the last '@'; anything before it is userinfo.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	switch {
	case strings.HasPrefix(s, "["):
		if i := strings.Index(s, "]"); i >= 0 {
			s = s[1:i]
		}
	case strings.Count(s, ":") > 1:
		// A bare IPv6 address (no brackets, no port).
	default:
		if i := strings.Index(s, ":"); i >= 0 {
			s = s[:i]
		}
	}
	if i := strings.Index(s, "%25"); i >= 0 {
		s = s[:i] + "%" + s[i+3:]
	}
	return strings.TrimSuffix(s, ".")
}

// localName reports whether host is a name that always means this machine.
// The "localhost" prefix match is kept from the original blocklist.
func localName(host string) bool {
	return strings.HasPrefix(host, "localhost") || strings.HasSuffix(host, ".localhost")
}

// HostDenyReason classifies host without resolving it: a local name or a
// barred IP literal returns a reason, and literal reports whether host was an
// IP literal at all (a literal needs no lookup).
func HostDenyReason(host string) (reason string, literal bool) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return ReasonUnspecified, false
	}
	if localName(h) {
		return ReasonLocalName, false
	}
	if addr, ok := ParseHostLiteral(h); ok {
		return DenyReason(addr), true
	}
	if numericDotted(h) {
		return ReasonNumericHost, false
	}
	return "", false
}

// numericDotted reports whether host has at least one dot and every label is
// a decimal number or a 0x hex number, the shapes inet_aton reads as IPv4
// parts. A host like that which ParseHostLiteral rejected (1.2.3.4.5,
// 256.1.1.1, 08.0.0.1) is not a name any public service has, so it is refused
// rather than handed to DNS.
func numericDotted(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !numericLabel(label) {
			return false
		}
	}
	return true
}

func numericLabel(label string) bool {
	digits := label
	isDigit := func(c rune) bool { return c >= '0' && c <= '9' }
	if strings.HasPrefix(label, "0x") {
		digits = label[len("0x"):]
		isDigit = func(c rune) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }
	}
	if digits == "" {
		// "0x" alone is zero to inet_aton; an empty label is not a number.
		return label != ""
	}
	for _, c := range digits {
		if !isDigit(c) {
			return false
		}
	}
	return true
}

// Resolver returns the addresses a host name resolves to, as text.
type Resolver func(ctx context.Context, host string) ([]string, error)

// IsPrivateURL reports whether rawURL targets a non-globally-routable
// destination. It fails closed: an empty host, a resolver error, no answers,
// or an answer that does not parse as an IP address all count as private, and
// one barred answer among several is enough to refuse.
func IsPrivateURL(ctx context.Context, rawURL string, resolve Resolver) bool {
	host := URLHost(rawURL)
	reason, literal := HostDenyReason(host)
	if reason != "" {
		return true
	}
	if literal {
		return false
	}
	if resolve == nil {
		return true
	}
	answers, err := resolve(ctx, host)
	if err != nil || len(answers) == 0 {
		return true
	}
	for _, a := range answers {
		addr, ok := ParseHostLiteral(a)
		if !ok || Denied(addr) {
			return true
		}
	}
	return false
}

package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
)

// The plain-HTTP (non-CONNECT) forward path is the one egress path through
// this proxy that carried NO destination policy at all: handleConn hands every
// non-CONNECT request to forwardPlainDirect, which RoundTripped it unchanged.
// An agent that sets a proxy URL by hand — or simply asks its CLI for
// `http://<anything>` — therefore had the proxy open a connection on its
// behalf, from the PROXY's uid, to any address the agent named. The forced
// egress gate does not cover it: the iptables rule only redirects :443, so
// plain HTTP is reachable only through this path, and the proxy's own uid is
// exempted from the redirect precisely so its upstream dials work.
//
// The interesting targets are not on the public internet, where the agent's
// own egress would have reached anyway. They are the addresses only the pod
// can see: the hive's own loopback services beside the proxy (the inference
// translator on :18444, the decision endpoint of hivecommons/hive#8939, the
// dashboard/API), the cloud instance-metadata service on 169.254.169.254
// (node credentials on most clouds), and the cluster-internal RFC1918 estate
// (the Kubernetes API, other spokes, internal registries).
//
// So the plain forward path dials only globally routable addresses. The check
// runs in the dialer's Control hook rather than on the request's host string:
// Control is called AFTER name resolution with the concrete remote address, so
// a hostname that resolves to 127.0.0.1 or 169.254.169.254 — a public DNS
// record pointed inward, the classic SSRF/DNS-rebinding shape — is refused on
// exactly the same evidence as a literal IP, and the connection that is
// actually opened is the one that was checked.

// plainForwardAllowPrivateEnv lifts the destination guard for deployments that
// genuinely egress to a private network over plain HTTP (a LAN mirror or an
// internal registry on a native/laptop install, where no cluster estate sits
// behind the proxy to protect). Boot-time operator choice, read once at
// construction, same shape as HIVE_PROXY_ADVISORY_OK: only the exact string
// "true" turns it on.
const plainForwardAllowPrivateEnv = "HIVE_PROXY_ALLOW_PRIVATE_FORWARD"

func plainForwardAllowPrivate() bool {
	return strings.TrimSpace(os.Getenv(plainForwardAllowPrivateEnv)) == "true"
}

// Destination classes refused on the plain forward path. The reason strings are
// user-facing: they appear in the 403 body an agent's curl prints, so they name
// what was refused rather than a policy code.
const (
	plainForwardReasonLoopback    = "loopback"
	plainForwardReasonLinkLocal   = "link-local"
	plainForwardReasonMulticast   = "multicast"
	plainForwardReasonPrivate     = "private"
	plainForwardReasonCGNAT       = "carrier-grade NAT"
	plainForwardReasonUnspecified = "unspecified"
)

// cgnatPrefix is RFC 6598 100.64.0.0/10. It is neither RFC 1918 (so
// netip.Addr.IsPrivate does not report it) nor routable on the public
// internet; several managed Kubernetes platforms number pod and service
// networks out of it, which is exactly the estate this guard protects.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// plainForwardDenied is the dial-time refusal, returned from the Control hook
// so http.Transport aborts the dial. forwardPlainDirect recovers it with
// errors.As to answer 403 (a policy refusal the agent can act on) instead of
// 502 (an upstream that failed).
type plainForwardDenied struct {
	target string
	reason string
}

func (e *plainForwardDenied) Error() string {
	return fmt.Sprintf("plain HTTP forward to %s refused: %s address", e.target, e.reason)
}

// plainForwardDenyReasonAddr names the class that bars an address, or "" when
// the address is globally routable and may be dialled.
func plainForwardDenyReasonAddr(addr netip.Addr) string {
	// An IPv4-mapped IPv6 address (::ffff:127.0.0.1) is the same host as its
	// IPv4 form; unmap so one rule covers both spellings.
	addr = addr.Unmap()
	switch {
	case !addr.IsValid(), addr.IsUnspecified():
		return plainForwardReasonUnspecified
	case addr.IsLoopback():
		return plainForwardReasonLoopback
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10 — cloud instance metadata lives here.
		return plainForwardReasonLinkLocal
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		return plainForwardReasonMulticast
	case addr.IsPrivate():
		// RFC 1918 for IPv4, fc00::/7 (unique local) for IPv6.
		return plainForwardReasonPrivate
	case cgnatPrefix.Contains(addr):
		return plainForwardReasonCGNAT
	}
	return ""
}

// plainForwardDenyReason classifies the "ip:port" the dialer is about to
// connect to. An address that does not parse as a literal IP cannot be
// classified and is refused: Control is always handed a resolved address, so
// an unparseable one means the policy could not be applied, and the deny
// direction is the safe one.
func plainForwardDenyReason(address string) string {
	host := address
	if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return plainForwardReasonUnspecified
	}
	return plainForwardDenyReasonAddr(addr)
}

// guardedPlainForwardTransport and unguardedPlainForwardTransport are built
// once and shared: a per-request Transport would drop keep-alives and leak
// idle connections. Which one a forward uses is the proxy's
// allowPrivateForward choice, so the policy stays a property of the
// deployment, not of the request.
var (
	guardedPlainForwardTransport   = newPlainForwardTransport(true)
	unguardedPlainForwardTransport = newPlainForwardTransport(false)
)

// newPlainForwardTransport clones the standard transport (keeping its
// connection pooling and timeouts), drops its ProxyFromEnvironment so an
// HTTP_PROXY/HTTPS_PROXY on the proxy process is deliberately ignored — with
// it the dial-time check would see the upstream proxy's address instead of the
// agent-named destination and evaluate the wrong peer — and replaces the dialer so every plain forward is SO_MARKed like the proxy's
// other upstream dials — without the mark, forwarding `http://host:443` dials
// :443 and the forced-egress redirect sends it straight back into this proxy —
// and, when guarded, destination-checked at resolution time.
func newPlainForwardTransport(guard bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := markDialer(upstreamDialTimeout)
		if guard {
			mark := dialer.Control
			dialer.Control = func(network, address string, c syscall.RawConn) error {
				if reason := plainForwardDenyReason(address); reason != "" {
					return &plainForwardDenied{target: address, reason: reason}
				}
				return mark(network, address, c)
			}
		}
		return dialer.DialContext(ctx, network, address)
	}
	return transport
}

// plainForwardTransport returns the transport this proxy forwards plain HTTP
// through. The zero value of allowPrivateForward is the guarded one, so a
// GitHubProxy built as a struct literal (tests, embedders) is protected
// without opting in.
func (p *GitHubProxy) plainForwardTransport() http.RoundTripper {
	if p.allowPrivateForward {
		return unguardedPlainForwardTransport
	}
	return guardedPlainForwardTransport
}

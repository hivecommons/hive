package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// egressMaxRedirects bounds every redirect chain the proxy's own outbound
// clients follow (inference forwarding, model discovery, the entitlement probe
// and the reference_repos lookups). It matches the bound the dashboard and
// knowledge clients in src/pkg already apply (ssrfMaxRedirects, 3), instead of
// net/http's default of 10 (hivecommons/hive#19222).
const egressMaxRedirects = 3

// egressCredentialHeaders are the headers the proxy's outbound clients carry
// credentials or tenant identity in. They are removed from any redirect hop
// that leaves the original host or downgrades https to http.
var egressCredentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "X-IBM-Project-ID", "X-Api-Key"}

// egressCheckRedirect returns the CheckRedirect policy for the proxy's own
// outbound clients. extraSensitive names further headers (a route's
// ExtraHeaders) to strip alongside egressCredentialHeaders.
//
// net/http on its own is not enough here: it keeps Authorization on a hop to
// the same domain on another port or to a subdomain, never strips custom
// headers such as X-IBM-Project-ID, and follows https -> http downgrades with
// every header intact. Comparing the full Host (port included) keeps the
// legitimate same-origin redirects (path normalization, trailing slash)
// working with credentials, which is what upstreams actually do.
//
// Only http(s) targets are followed, and the chain is cut after
// egressMaxRedirects hops; both fail closed with an error from client.Do.
func egressCheckRedirect(extraSensitive ...string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= egressMaxRedirects {
			return fmt.Errorf("stopped after %d redirects", egressMaxRedirects)
		}
		if scheme := strings.ToLower(req.URL.Scheme); scheme != "http" && scheme != "https" {
			return fmt.Errorf("redirect to non-http(s) scheme blocked: %s", req.URL.Scheme)
		}
		if len(via) == 0 {
			return nil
		}
		first := via[0].URL
		if !strings.EqualFold(req.URL.Host, first.Host) && !egressHostPrivate(req.Context(), first.Hostname()) {
			// A hop to another host from a public endpoint must not land on
			// a private, loopback, link-local or metadata address. An
			// original endpoint that is itself private (an in-cluster model
			// backend) may redirect within the private estate.
			if reason := egressHostDenyReason(req.Context(), req.URL.Hostname()); reason != "" {
				return fmt.Errorf("redirect to %s address blocked: %s", reason, req.URL.Host)
			}
		}
		downgrade := strings.EqualFold(first.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https")
		if !downgrade && strings.EqualFold(req.URL.Host, first.Host) {
			return nil
		}
		for _, h := range egressCredentialHeaders {
			req.Header.Del(h)
		}
		for _, h := range extraSensitive {
			if h != "" {
				req.Header.Del(h)
			}
		}
		return nil
	}
}

// routeSensitiveHeaders lists the header names a route adds on top of
// Authorization, so a cross-host redirect strips them too.
func routeSensitiveHeaders(route *InferenceRoute) []string {
	if route == nil || len(route.ExtraHeaders) == 0 {
		return nil
	}
	out := make([]string, 0, len(route.ExtraHeaders))
	for k := range route.ExtraHeaders {
		out = append(out, k)
	}
	return out
}

// egressResolveHost resolves a host name for the redirect guard. A package var
// so tests can answer without DNS.
var egressResolveHost = func(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	return ips, err
}

// egressHostDenyReason classifies a redirect target with the proxy's shared
// address classifier (plainForwardDenyReasonAddr). It fails closed: an empty
// host, a resolve error or no answers is refused, and one barred answer among
// several is enough.
func egressHostDenyReason(ctx context.Context, host string) string {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return plainForwardReasonUnspecified
	}
	if lh := strings.ToLower(host); lh == "localhost" || strings.HasSuffix(lh, ".localhost") {
		return plainForwardReasonLoopback
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return plainForwardDenyReasonAddr(addr)
	}
	addrs, err := egressResolveHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return "unresolvable"
	}
	for _, a := range addrs {
		if r := plainForwardDenyReasonAddr(a); r != "" {
			return r
		}
	}
	return ""
}

// egressHostPrivate reports whether the original endpoint is itself private.
// A host that cannot be classified counts as public, so the guard stays on.
func egressHostPrivate(ctx context.Context, host string) bool {
	r := egressHostDenyReason(ctx, host)
	return r != "" && r != "unresolvable" && r != plainForwardReasonUnspecified
}

package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrRefused marks a dial PinnedDialer would not make. The text matches the
// redirect guards' "private/internal host blocked" so callers and logs read
// the same whichever check fired.
var ErrRefused = errors.New("private/internal host blocked")

// DialFunc opens a connection to addr ("host:port").
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// PinnedDialer closes the rebinding window between "check the URL" and "dial
// the URL" for a server-side fetch (#18992, part of #18986). A pre-fetch
// IsPrivateURL check resolves the name, and http.Client then resolves it again
// when it dials, so a name whose answer changes between the two lookups (DNS
// rebinding, a TTL of zero) is checked as public and connected to as private.
// PinnedDialer resolves the dial target exactly once, refuses the dial when
// any answer is barred (not only the first), and connects to an address it
// checked, never to the name. As a Transport's DialContext it runs for every
// new connection, so every redirect hop is checked at dial time with the same
// classifier, whatever the CheckRedirect policy does.
//
// This is the shape of the agent egress proxy's dial-time guard
// (src/crux/proxy/plain_forward_guard.go) and of inbox.LinkFetcher.dial.
type PinnedDialer struct {
	// Resolve looks a name up. Nil means net.DefaultResolver.LookupHost.
	// A lookup error or an empty answer refuses the dial (fail closed).
	Resolve Resolver
	// Refuse reports whether the connection to target, an "ip:port" the
	// dialer has already resolved, must not be made. Nil means the address
	// is refused when DenyReason names a class for it. Callers with a test
	// seam on their pre-fetch guard pass that guard here, so the dial-time
	// check and the pre-fetch check are the same function.
	Refuse func(ctx context.Context, target string) bool
	// Dial connects to a checked "ip:port". Nil means a net.Dialer with a
	// 30s timeout.
	Dial DialFunc
}

func (p PinnedDialer) resolve(ctx context.Context, host string) ([]string, error) {
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

func (p PinnedDialer) refuse(ctx context.Context, target string) bool {
	if p.Refuse != nil {
		return p.Refuse(ctx, target)
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return true
	}
	addr, ok := ParseHostLiteral(host)
	return !ok || Denied(addr)
}

func (p PinnedDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// Targets resolves addr's host once and returns the "ip:port" targets the
// dialer may connect to, in answer order. It refuses the whole set when any
// answer is barred or does not parse as an address, when the lookup fails,
// or when it returns nothing.
func (p PinnedDialer) Targets(ctx context.Context, addr string) ([]string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	var answers []string
	if lit, ok := ParseHostLiteral(host); ok {
		answers = []string{lit.String()}
	} else {
		answers, err = p.resolve(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: looking up %s: %v", ErrRefused, host, err)
		}
		if len(answers) == 0 {
			return nil, fmt.Errorf("%w: looking up %s: no addresses", ErrRefused, host)
		}
	}
	targets := make([]string, 0, len(answers))
	for _, a := range answers {
		ip, ok := ParseHostLiteral(a)
		if !ok {
			return nil, fmt.Errorf("%w: %s resolved to unparseable answer %q", ErrRefused, host, a)
		}
		target := net.JoinHostPort(ip.Unmap().String(), port)
		if p.refuse(ctx, target) {
			return nil, fmt.Errorf("%w: %s resolves to %s", ErrRefused, host, ip)
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// DialContext resolves addr once, checks every answer, and connects to the
// checked addresses in order until one answers.
func (p PinnedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	targets, err := p.Targets(ctx, addr)
	if err != nil {
		return nil, err
	}
	var firstErr error
	for _, t := range targets {
		conn, err := p.dial(ctx, network, t)
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// Transport returns an http.Transport whose every new connection goes
// through p.DialContext.
//
// Chained upstream decision: the transport keeps the standard
// HTTP(S)_PROXY / NO_PROXY behaviour of http.DefaultTransport. When a forward
// proxy applies, the connection is to the operator-configured proxy, which is
// dialled as configured (it is commonly on a private address), and the proxy
// resolves the target name itself. Hive cannot pin that resolution, the same
// documented limit as dialUpstreamRaw in the agent egress proxy, so on a
// proxied fetch the pre-fetch check and the CheckRedirect policy are the
// controls. Without a proxy (the default) every dial is pinned.
func (p PinnedDialer) Transport() *http.Transport {
	var proxies sync.Map // canonical "host:port" of every proxy URL handed out
	proxy := func(req *http.Request) (*url.URL, error) {
		u, err := http.ProxyFromEnvironment(req)
		if u != nil && err == nil {
			proxies.Store(proxyHostPort(u), struct{}{})
		}
		return u, err
	}
	return &http.Transport{
		Proxy: proxy,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, ok := proxies.Load(strings.ToLower(addr)); ok {
				return p.dial(ctx, network, addr)
			}
			return p.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// proxyHostPort is the address http.Transport dials for proxy u.
func proxyHostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return strings.ToLower(net.JoinHostPort(u.Hostname(), port))
}

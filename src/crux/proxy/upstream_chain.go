package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// SetUpstreamProxy installs the rule set deciding which upstream hosts this
// proxy chains through a corporate HTTP forward proxy (CONNECT) instead of
// dialling directly. nil (the default) keeps every upstream dial direct. The
// motivating case is a GitHub Enterprise host reachable only via an
// enterprise egress proxy: agents' git/gh traffic is iptables-redirected into
// this proxy, so this is the one place the chaining can happen for them.
func (p *GitHubProxy) SetUpstreamProxy(sel *upstreamproxy.Selector) {
	p.upstream = sel
	if sel != nil && p.logger != nil {
		p.logger.Info("proxy: upstream proxy chaining enabled",
			"proxy", redactedProxyURL(sel), "source", sel.Source(), "restricted", sel.Restricted())
	}
}

func redactedProxyURL(sel *upstreamproxy.Selector) string {
	u := sel.URL()
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// dialUpstreamTLS opens the TLS leg to hostport for a MITM'd flow, presenting
// serverName for SNI/verification. When the upstream selector names the host
// the TCP leg is a CONNECT tunnel through the upstream proxy; otherwise it is
// the direct SO_MARKed dial used before chaining existed. timeout bounds
// connect (+ CONNECT exchange) + TLS handshake together, as
// tls.DialWithDialer did.
func (p *GitHubProxy) dialUpstreamTLS(hostport, serverName string, timeout time.Duration) (net.Conn, error) {
	proxyURL := p.upstream.ProxyFor(hostport)
	if proxyURL == nil {
		return tls.DialWithDialer(markDialer(timeout), "tcp", hostport, &tls.Config{ServerName: serverName})
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, err := upstreamproxy.Dial(ctx, markDialer(timeout).DialContext, proxyURL, hostport)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{ServerName: serverName})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return tlsConn, nil
}

// dialUpstreamRaw opens the raw TCP leg of an uninspected tunnel (CONNECT
// passthrough or transparent-TLS passthrough) to hostport: through the
// upstream proxy when the selector names the host, otherwise with the
// destination-guarded direct dialer. On the chained path the proxy resolves
// the name, so the guard can only classify a literal IP destination; it does
// so before handing the target to the proxy.
func (p *GitHubProxy) dialUpstreamRaw(hostport string, timeout time.Duration) (net.Conn, error) {
	proxyURL := p.upstream.ProxyFor(hostport)
	if proxyURL == nil {
		return p.tunnelDialer(timeout).Dial("tcp", hostport)
	}
	if !p.allowPrivateForward {
		if host, _, err := net.SplitHostPort(hostport); err == nil {
			if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
				if reason := plainForwardDenyReasonAddr(addr); reason != "" {
					return nil, &plainForwardDenied{target: hostport, reason: reason}
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return upstreamproxy.Dial(ctx, markDialer(timeout).DialContext, proxyURL, hostport)
}

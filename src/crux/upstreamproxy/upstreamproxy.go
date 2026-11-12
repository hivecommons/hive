// Package upstreamproxy decides which outbound connections must be chained
// through a corporate HTTP forward proxy, and opens them.
//
// The motivating deployment is a hive whose GitHub Enterprise host is reachable
// only through an enterprise egress proxy while everything else (github.com,
// model providers, the hub) is reached directly. Every :443 egress from a hive
// pod is iptables-redirected into the in-process MITM proxy (crux/proxy), which
// used to dial upstream directly; this package gives that proxy — and the
// control plane's own GitHub clients — a single rule set answering "does this
// host go via the upstream proxy, and if so which one?".
//
// Resolution order (Resolve): the hive config block `network.upstream_proxy`
// (url / hosts / no_proxy), each field overridable by HIVE_UPSTREAM_PROXY_URL,
// HIVE_UPSTREAM_PROXY_HOSTS and HIVE_UPSTREAM_PROXY_NO_PROXY; when no URL is
// configured that way the standard HTTPS_PROXY / HTTP_PROXY / NO_PROXY
// environment is honoured with http.ProxyFromEnvironment semantics (every host
// not excluded by NO_PROXY). Config wins over the standard environment.
package upstreamproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Environment variable names. HIVE_UPSTREAM_PROXY_HOSTS and
// HIVE_UPSTREAM_PROXY_NO_PROXY are comma-separated lists of host patterns.
const (
	URLEnv     = "HIVE_UPSTREAM_PROXY_URL"
	HostsEnv   = "HIVE_UPSTREAM_PROXY_HOSTS"
	NoProxyEnv = "HIVE_UPSTREAM_PROXY_NO_PROXY"
)

// Config is the operator-facing rule set (the `network.upstream_proxy` block).
type Config struct {
	// URL is the forward proxy, e.g. "http://proxy.corp.example:3128".
	// Userinfo, when present, is sent as Proxy-Authorization: Basic.
	URL string
	// Hosts lists the destination host patterns that must go via the proxy.
	// Empty means every host not matched by NoProxy. Patterns: an exact host
	// ("github.corp.example"), a wildcard ("*.corp.example" — subdomains
	// only), a dotted suffix (".corp.example" — the domain and its
	// subdomains), an IP literal, a CIDR, or "*".
	Hosts []string
	// NoProxy lists host patterns that are always dialled directly, with the
	// same pattern grammar. Loopback is direct unless a Hosts rule names it
	// explicitly (an IP literal or CIDR).
	NoProxy []string
}

// Selector answers ProxyFor for one resolved rule set. A nil *Selector is
// valid and means "no upstream proxy": every method on it reports direct.
type Selector struct {
	proxyURL *url.URL
	hosts    []hostRule
	noProxy  []hostRule
	source   string
}

// Resolve builds the selector from cfg overlaid with the process environment
// (getenv; nil means the environment is not consulted). It returns (nil, nil)
// when nothing configures an upstream proxy, and an error only for a
// malformed proxy URL or pattern — the caller logs it and runs direct.
func Resolve(cfg Config, getenv func(string) string) (*Selector, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	source := "config"
	rawURL := strings.TrimSpace(cfg.URL)
	hosts := cfg.Hosts
	noProxy := cfg.NoProxy
	if v := strings.TrimSpace(getenv(URLEnv)); v != "" {
		rawURL = v
	}
	if v, ok := envList(getenv, HostsEnv); ok {
		hosts = v
	}
	if v, ok := envList(getenv, NoProxyEnv); ok {
		noProxy = v
	}
	if rawURL == "" {
		// Standard environment fallback: HTTPS_PROXY (then HTTP_PROXY) applies
		// to every host NO_PROXY does not exclude.
		source = "env"
		hosts = nil
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if v := strings.TrimSpace(getenv(name)); v != "" {
				rawURL = v
				break
			}
		}
		if rawURL == "" {
			return nil, nil
		}
		if _, explicit := envList(getenv, NoProxyEnv); !explicit {
			noProxy = nil
			for _, name := range []string{"NO_PROXY", "no_proxy"} {
				if v, ok := envList(getenv, name); ok {
					noProxy = v
					break
				}
			}
		}
	}
	return New(rawURL, hosts, noProxy, source)
}

// New builds a selector from already-resolved values. source labels where
// the rules came from ("config" or "env") for logging.
func New(rawURL string, hosts, noProxy []string, source string) (*Selector, error) {
	u, err := parseProxyURL(rawURL)
	if err != nil {
		return nil, err
	}
	hostRules, err := parseRules(hosts)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy hosts: %w", err)
	}
	noProxyRules, err := parseRules(noProxy)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy no_proxy: %w", err)
	}
	if source == "" {
		source = "config"
	}
	return &Selector{proxyURL: u, hosts: hostRules, noProxy: noProxyRules, source: source}, nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("upstream proxy url is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	// Errors are logged by the caller; the URL's userinfo is the proxy's
	// Basic credential, so never echo raw.
	u, err := url.Parse(raw)
	if err != nil {
		// *url.Error.Error() repeats the full URL; keep only its inner cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("upstream proxy url %q: %w", redactUserinfo(raw), err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("upstream proxy url %q: scheme %q not supported (http or https)", u.Redacted(), u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("upstream proxy url %q: missing host", u.Redacted())
	}
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	return u, nil
}

// redactUserinfo masks everything between "://" and the first "@" of raw, for
// URLs url.Parse rejected and so cannot be redacted structurally.
func redactUserinfo(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return raw
	}
	return raw[:i+3] + "xxxxx@" + rest[at+1:]
}

func envList(getenv func(string) string, name string) ([]string, bool) {
	raw := getenv(name)
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, true
}

// URL returns the configured proxy URL, or nil for a nil selector.
func (s *Selector) URL() *url.URL {
	if s == nil {
		return nil
	}
	return s.proxyURL
}

// Source reports where the rules came from: "config", "env", or "" (none).
func (s *Selector) Source() string {
	if s == nil {
		return ""
	}
	return s.source
}

// Restricted reports whether the selector names specific hosts (true) or
// applies to every non-excluded host (false).
func (s *Selector) Restricted() bool {
	return s != nil && len(s.hosts) > 0
}

// ProxyFor returns the proxy a connection to hostport ("host" or
// "host:port") must be chained through, or nil to dial directly.
func (s *Selector) ProxyFor(hostport string) *url.URL {
	if s == nil || s.proxyURL == nil {
		return nil
	}
	host := normalizeHost(hostport)
	if host == "" {
		return nil
	}
	for _, r := range s.noProxy {
		if r.matches(host) {
			return nil
		}
	}
	loopback := isLoopbackHost(host)
	if len(s.hosts) == 0 {
		if loopback {
			return nil
		}
		return s.proxyURL
	}
	for _, r := range s.hosts {
		if r.matches(host) && !(loopback && r.kind == ruleAll) {
			return s.proxyURL
		}
	}
	return nil
}

// ProxyFunc adapts the selector to http.Transport.Proxy. A nil selector
// yields a function that always reports direct, so callers can assign it
// unconditionally.
func (s *Selector) ProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		return s.ProxyFor(req.URL.Host), nil
	}
}

// GitEnv returns the environment entries that make a child `git` process
// honour the same rules, expressed as GIT_CONFIG_COUNT/KEY/VALUE entries so
// nothing else in the child (or the parent's Go clients) is affected:
// `http.proxy` when the selector applies to every host, otherwise one
// `http.https://<host>/.proxy` entry per exact-host pattern. Wildcard, suffix,
// IP and CIDR patterns cannot be expressed per-URL in git config and are
// skipped (documented limitation). NoProxy is handed over as `no_proxy`.
func (s *Selector) GitEnv() []string {
	if s == nil || s.proxyURL == nil {
		return nil
	}
	var pairs [][2]string
	if len(s.hosts) == 0 {
		pairs = append(pairs, [2]string{"http.proxy", s.proxyURL.String()})
	} else {
		for _, r := range s.hosts {
			if r.kind == ruleExact {
				pairs = append(pairs, [2]string{"http.https://" + r.host + "/.proxy", s.proxyURL.String()})
			}
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs))}
	for i, kv := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	if len(s.noProxy) > 0 {
		raw := make([]string, 0, len(s.noProxy))
		for _, r := range s.noProxy {
			raw = append(raw, r.raw)
		}
		env = append(env, "no_proxy="+strings.Join(raw, ","))
	}
	return env
}

// Dialer is the context dial function the CONNECT leg uses to reach the
// proxy itself (the caller supplies its SO_MARKed dialer).
type Dialer func(ctx context.Context, network, address string) (net.Conn, error)

// connectResponseTimeout bounds how long the proxy may take to answer the
// CONNECT once the TCP leg is up; the caller's dial timeout bounds the leg.
const connectResponseTimeout = 15 * time.Second

// Dial opens a TCP tunnel to target ("host:port") through proxyURL: it dials
// the proxy with dial, issues `CONNECT target`, and returns the connection
// positioned after the proxy's 200 response so the caller can run TLS (or
// splice bytes) over it exactly as it would over a direct connection.
func Dial(ctx context.Context, dial Dialer, proxyURL *url.URL, target string) (net.Conn, error) {
	if proxyURL == nil {
		return nil, errors.New("upstream proxy: nil proxy URL")
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, fmt.Errorf("upstream proxy: target %q must be host:port: %w", target, err)
	}
	raw, err := dial(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy %s: dial: %w", proxyURL.Host, err)
	}
	conn := raw
	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(raw, &tls.Config{ServerName: proxyURL.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("upstream proxy %s: tls: %w", proxyURL.Host, err)
		}
		conn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{},
	}
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + pass))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}

	deadline := time.Now().Add(connectResponseTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy %s: write CONNECT: %w", proxyURL.Host, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy %s: read CONNECT response: %w", proxyURL.Host, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy %s: CONNECT %s refused: %s", proxyURL.Host, target, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// The proxy sent bytes past its response header (unusual: the client
		// speaks first in every protocol tunnelled here). Preserve them.
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ---- host rules ------------------------------------------------------------

type ruleKind int

const (
	ruleAll ruleKind = iota
	ruleExact
	ruleSubdomains // *.example.com — subdomains only
	ruleSuffix     // .example.com — the domain and its subdomains
	ruleIP
	ruleCIDR
)

type hostRule struct {
	raw    string
	kind   ruleKind
	host   string
	ip     netip.Addr
	prefix netip.Prefix
}

func parseRules(patterns []string) ([]hostRule, error) {
	rules := make([]hostRule, 0, len(patterns))
	for _, p := range patterns {
		raw := strings.TrimSpace(p)
		if raw == "" {
			continue
		}
		r, err := parseRule(raw)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func parseRule(raw string) (hostRule, error) {
	r := hostRule{raw: raw}
	p := strings.ToLower(raw)
	if p == "*" {
		r.kind = ruleAll
		return r, nil
	}
	if strings.Contains(p, "/") {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return r, fmt.Errorf("pattern %q: %w", raw, err)
		}
		r.kind, r.prefix = ruleCIDR, prefix.Masked()
		return r, nil
	}
	if addr, err := netip.ParseAddr(strings.Trim(p, "[]")); err == nil {
		r.kind, r.ip = ruleIP, addr.Unmap()
		return r, nil
	}
	// Strip a :port a NO_PROXY-style entry may carry; ports are not matched.
	if h, _, err := net.SplitHostPort(p); err == nil {
		p = h
	}
	switch {
	case strings.HasPrefix(p, "*."):
		r.kind, r.host = ruleSubdomains, strings.TrimSuffix(p[2:], ".")
	case strings.HasPrefix(p, "."):
		r.kind, r.host = ruleSuffix, strings.TrimSuffix(p[1:], ".")
	default:
		r.kind, r.host = ruleExact, strings.TrimSuffix(p, ".")
	}
	if r.host == "" || strings.ContainsAny(r.host, "*/ ") {
		return r, fmt.Errorf("pattern %q: not a host, wildcard, suffix, IP or CIDR", raw)
	}
	return r, nil
}

func (r hostRule) matches(host string) bool {
	switch r.kind {
	case ruleAll:
		return true
	case ruleExact:
		return host == r.host
	case ruleSubdomains:
		return strings.HasSuffix(host, "."+r.host)
	case ruleSuffix:
		return host == r.host || strings.HasSuffix(host, "."+r.host)
	case ruleIP:
		addr, err := netip.ParseAddr(host)
		return err == nil && addr.Unmap() == r.ip
	case ruleCIDR:
		addr, err := netip.ParseAddr(host)
		return err == nil && r.prefix.Contains(addr.Unmap())
	}
	return false
}

// normalizeHost lowercases hostport, strips a port and brackets, and drops a
// trailing dot.
func normalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// ---- process-wide default --------------------------------------------------

var (
	defaultMu       sync.RWMutex
	defaultSelector *Selector
)

// SetDefault installs the process-wide selector the hive resolved at boot;
// nil clears it. In-process git execs (GitEnv) read it so a clone of the GHE
// host from the control plane follows the same rules as the MITM proxy.
func SetDefault(s *Selector) {
	defaultMu.Lock()
	defaultSelector = s
	defaultMu.Unlock()
}

// Default returns the process-wide selector, or nil when none is installed.
func Default() *Selector {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultSelector
}

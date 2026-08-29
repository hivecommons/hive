package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func init() {
	RegisterResolver("http", newHTTPResolver)
}

// maxHTTPResponseBytes caps how much of an http resolver's response body is read
// into a variable value. Large enough for a config value or token, small enough
// that a hostile endpoint cannot exhaust memory.
const maxHTTPResponseBytes = 64 * 1024

// httpResolver resolves a variable to the body of an HTTP GET.
//
// Security model:
//   - Gated: refuses to build unless Policy.AllowHTTP is true (seed-only, like
//     allow_exec) AND a non-empty host allowlist is configured.
//   - Allowlisted host: the request host must be in variables.security.http_allow
//     list.
//   - SSRF/rebind defense: the connection dials a custom control that, at dial
//     time (after DNS resolution), rejects any target IP that is loopback,
//     link-local, or private (RFC1918/ULA) unless the operator explicitly
//     allowlisted that host — this blocks DNS-rebinding to internal addresses.
//   - GET only, no automatic redirect following (a redirect to a non-allowlisted
//     host would bypass the check), a response-size cap, and a request timeout.
//   - Header values may contain ${ENV} references so tokens come from the
//     environment, never from YAML.
type httpResolver struct {
	url       string
	headers   map[string]string
	allowlist map[string]struct{}
	client    *http.Client
	dflt      string
	hasDflt   bool
	tag       string
}

func newHTTPResolver(spec VarSpec, pol Policy) (Resolver, error) {
	if !pol.AllowHTTP {
		return nil, errors.New("http resolvers are disabled (set variables.security.allow_http in the seed config to enable)")
	}
	if len(pol.HTTPAllowlist) == 0 {
		return nil, errors.New("http resolvers require a non-empty variables.security.http_allowlist")
	}
	if spec.URL == "" {
		return nil, fmt.Errorf("http variable %q has no url", spec.Name)
	}
	u, err := url.Parse(spec.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("http variable %q has an invalid url", spec.Name)
	}
	allow := map[string]struct{}{}
	for _, h := range pol.HTTPAllowlist {
		allow[strings.ToLower(h)] = struct{}{}
	}
	if _, ok := allow[strings.ToLower(u.Hostname())]; !ok {
		return nil, fmt.Errorf("http variable %q host %q is not in http_allowlist", spec.Name, u.Hostname())
	}

	timeout := time.Duration(pol.HTTPTimeoutS) * time.Second
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		// Reject dialing to private/loopback/link-local IPs (post-DNS), which
		// defeats DNS-rebinding to internal targets. The host is already
		// allowlisted; this guards the resolved ADDRESS.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip := net.ParseIP(host); ip != nil && isDisallowedIP(ip) {
				return nil, fmt.Errorf("resolve: refusing to dial private/loopback address %s", host)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// Do not follow redirects — a redirect could send us to a
		// non-allowlisted or internal host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &httpResolver{
		url:       spec.URL,
		headers:   spec.Headers,
		allowlist: allow,
		client:    client,
		dflt:      spec.Default,
		hasDflt:   spec.HasDefault,
		tag:       "http:" + strings.ToLower(u.Hostname()),
	}, nil
}

func (r *httpResolver) Resolve(req Request) (Result, error) {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return r.fallback()
	}
	for k, v := range r.headers {
		// Expand ${ENV} in header values so tokens come from the environment.
		hreq.Header.Set(k, expandEnvOnly(v))
	}

	resp, err := r.client.Do(hreq)
	if err != nil {
		return r.fallback()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return r.fallback()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseBytes))
	if err != nil {
		return r.fallback()
	}
	return Result{Value: strings.TrimRight(string(body), "\n"), Source: r.tag}, nil
}

func (r *httpResolver) fallback() (Result, error) {
	if r.hasDflt {
		return Result{Value: r.dflt, Source: "default"}, nil
	}
	return Result{}, ErrUnresolved
}

// expandEnvOnly substitutes ${NAME} with os.Getenv(NAME), leaving unset
// variables literal. Used for http header values so a secret token is read from
// the environment rather than written in YAML.
func expandEnvOnly(s string) string {
	return VarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return match
	})
}

// allowPrivateDial, when true, disables the private/loopback dial guard. It is
// ONLY set by tests (httptest binds loopback); it is never exposed or set in
// production code, so the SSRF protection is always on at runtime.
var allowPrivateDial = false

// isDisallowedIP reports whether an IP is one we refuse to dial: loopback,
// link-local (uni/multicast), unspecified, or private (RFC1918 / ULA fc00::/7).
func isDisallowedIP(ip net.IP) bool {
	if allowPrivateDial {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return ip.IsPrivate()
}

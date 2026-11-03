package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// plainForwardRoundTrip drives forwardPlainDirect over a pipe and returns the
// response the agent-side client would see. The handler side of the pipe is
// closed after the handler returns so the body read sees EOF (forwardPlainDirect
// leaves the conn open; handleConn owns its lifecycle in production).
func plainForwardRoundTrip(t *testing.T, p *GitHubProxy, req *http.Request) *http.Response {
	t.Helper()
	clientConn, proxyClient := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	go func() {
		p.forwardPlainDirect(proxyClient, req)
		_ = proxyClient.Close()
	}()

	_ = clientConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPlainForwardDenyReasonClassifiesDestinations(t *testing.T) {
	cases := map[string]string{
		// Refused: everything only the pod/node can reach.
		"127.0.0.1:80":          plainForwardReasonLoopback,
		"127.0.0.53:8080":       plainForwardReasonLoopback,
		"[::1]:80":              plainForwardReasonLoopback,
		"[::ffff:127.0.0.1]:80": plainForwardReasonLoopback,
		"169.254.169.254:80":    plainForwardReasonLinkLocal,
		"[fe80::1]:80":          plainForwardReasonLinkLocal,
		"10.0.0.5:80":           plainForwardReasonPrivate,
		"172.16.4.1:8080":       plainForwardReasonPrivate,
		"192.168.1.1:80":        plainForwardReasonPrivate,
		"[fd00::1]:80":          plainForwardReasonPrivate,
		"100.64.0.1:80":         plainForwardReasonCGNAT,
		// 224.0.0.0/24 (mDNS) is link-local multicast; 239.0.0.0/8 is not.
		"224.0.0.251:5353":     plainForwardReasonLinkLocal,
		"239.255.255.250:1900": plainForwardReasonMulticast,
		"[ff02::1]:80":         plainForwardReasonLinkLocal,
		"0.0.0.0:80":           plainForwardReasonUnspecified,
		"[::]:80":              plainForwardReasonUnspecified,
		"not-an-ip:80":         plainForwardReasonUnspecified,
		"192.168.1.1":          plainForwardReasonPrivate,
		// Allowed: globally routable.
		"93.184.216.34:80":        "",
		"[2606:2800:220:1::1]:80": "",
		"140.82.121.4:443":        "",
	}
	for address, want := range cases {
		if got := plainForwardDenyReason(address); got != want {
			t.Errorf("plainForwardDenyReason(%q) = %q, want %q", address, got, want)
		}
	}
}

// TestPlainForwardDenyReasonAddrUnmapsV4 pins the IPv4-mapped IPv6 case
// separately: ::ffff:10.0.0.1 is the private host 10.0.0.1 written the other
// way round, and netip's IsPrivate reports false until the address is unmapped.
func TestPlainForwardDenyReasonAddrUnmapsV4(t *testing.T) {
	mapped := netip.MustParseAddr("::ffff:10.0.0.1")
	if got := plainForwardDenyReasonAddr(mapped); got != plainForwardReasonPrivate {
		t.Errorf("plainForwardDenyReasonAddr(%v) = %q, want %q", mapped, got, plainForwardReasonPrivate)
	}
}

// TestForwardPlainDirectRefusesLoopbackDestination is the SSRF regression: a
// plain-HTTP request naming a service beside the proxy must be refused with 403
// and must never reach that service.
func TestForwardPlainDirectRefusesLoopbackDestination(t *testing.T) {
	reached := make(chan struct{}, 1)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	p := newTestProxy()
	req, err := http.NewRequest(http.MethodGet, internal.URL+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp := plainForwardRoundTrip(t, p, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a loopback destination", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), plainForwardReasonLoopback) {
		t.Errorf("body = %q, want the refused address class named", string(body))
	}
	select {
	case <-reached:
		t.Fatal("the loopback service was reached — the proxy forwarded a request it must refuse")
	default:
	}
}

// TestForwardPlainDirectRefusesHostnameResolvingInward covers the SSRF shape a
// host-string check misses: the request names a HOSTNAME, and only the resolved
// address is non-routable. "localhost" stands in for any public DNS record
// pointed at the loopback or metadata address.
func TestForwardPlainDirectRefusesHostnameResolvingInward(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}

	p := newTestProxy()
	req, err := http.NewRequest(http.MethodGet, "http://localhost:"+port+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp := plainForwardRoundTrip(t, p, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a hostname that resolves to loopback", resp.StatusCode)
	}
}

// TestForwardPlainDirectCountsNonLoopbackRefusalAsViolation separates the two
// refusal flavours: loopback is a client that ignored NO_PROXY (#14076) and is
// not held against the caller, while reaching for the private estate is.
func TestForwardPlainDirectCountsNonLoopbackRefusalAsViolation(t *testing.T) {
	p := newTestProxy()
	p.proxyAdvisoryOK = true // no UID map in tests; accept the header identity

	metadata, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	metadata.Header.Set("Proxy-Authorization", "hive scanner")

	if resp := plainForwardRoundTrip(t, p, metadata); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for the cloud metadata address", resp.StatusCode)
	}
	if got := p.violations["scanner"]; got != 1 {
		t.Errorf("violations[scanner] = %d, want 1 — a metadata-service attempt must be attributed and counted", got)
	}

	loopback, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	loopback.Header.Set("Proxy-Authorization", "hive scanner")

	if resp := plainForwardRoundTrip(t, p, loopback); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a loopback destination", resp.StatusCode)
	}
	if got := p.violations["scanner"]; got != 1 {
		t.Errorf("violations[scanner] = %d, want it unchanged at 1 — a loopback request is a client misconfiguration, not a violation", got)
	}
}

// TestForwardPlainDirectStripsProxyHopHeaders pins that the hop-by-hop headers
// addressed to THIS proxy do not travel upstream: Proxy-Authorization carries
// the caller's asserted identity (and, in the Basic form, a password field).
func TestForwardPlainDirectStripsProxyHopHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy()
	p.allowPrivateForward = true // the upstream under test is a loopback httptest server

	req, err := http.NewRequest(http.MethodGet, upstream.URL+"/ok", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Proxy-Authorization", "hive scanner")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("X-Keep", "me")

	if resp := plainForwardRoundTrip(t, p, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	headers := <-seen
	if got := headers.Get("Proxy-Authorization"); got != "" {
		t.Errorf("upstream saw Proxy-Authorization = %q, want it stripped", got)
	}
	if got := headers.Get("Proxy-Connection"); got != "" {
		t.Errorf("upstream saw Proxy-Connection = %q, want it stripped", got)
	}
	if got := headers.Get("X-Keep"); got != "me" {
		t.Errorf("upstream saw X-Keep = %q, want the request's own headers forwarded untouched", got)
	}
}

// TestNewGitHubProxyWiresPlainForwardEscapeHatch pins the operator opt-in: a
// deployment that genuinely egresses to a private network over plain HTTP can
// turn the guard off, and nothing else can.
func TestNewGitHubProxyWiresPlainForwardEscapeHatch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Setenv(plainForwardAllowPrivateEnv, "")
	guarded, err := NewGitHubProxyEphemeral(logger, "hivecommons", []string{"hive"})
	if err != nil {
		t.Fatalf("NewGitHubProxyEphemeral: %v", err)
	}
	if guarded.allowPrivateForward {
		t.Error("the plain-HTTP destination guard must be on when the opt-in is unset")
	}
	if guarded.plainForwardTransport() != http.RoundTripper(guardedPlainForwardTransport) {
		t.Error("a guarded proxy must forward through the guarded transport")
	}

	t.Setenv(plainForwardAllowPrivateEnv, "true")
	lifted, err := NewGitHubProxyEphemeral(logger, "hivecommons", []string{"hive"})
	if err != nil {
		t.Fatalf("NewGitHubProxyEphemeral: %v", err)
	}
	if !lifted.allowPrivateForward {
		t.Errorf("%s=true must lift the guard", plainForwardAllowPrivateEnv)
	}
	if lifted.plainForwardTransport() != http.RoundTripper(unguardedPlainForwardTransport) {
		t.Error("a proxy with the guard lifted must forward through the unguarded transport")
	}

	t.Setenv(plainForwardAllowPrivateEnv, "yes")
	fuzzy, err := NewGitHubProxyEphemeral(logger, "hivecommons", []string{"hive"})
	if err != nil {
		t.Fatalf("NewGitHubProxyEphemeral: %v", err)
	}
	if fuzzy.allowPrivateForward {
		t.Errorf("only the exact string \"true\" may lift the guard, not %q", "yes")
	}
}

// TestPlainForwardTransportIgnoresProxyEnvironment pins that the plain-forward
// transports dial the named destination directly, so the dial-time check never
// evaluates an upstream proxy's address.
func TestPlainForwardTransportIgnoresProxyEnvironment(t *testing.T) {
	for _, guard := range []bool{true, false} {
		if newPlainForwardTransport(guard).Proxy != nil {
			t.Errorf("newPlainForwardTransport(%v).Proxy is set, want nil", guard)
		}
	}

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	req, err := http.NewRequest(http.MethodGet, internal.URL+"/secret", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp := plainForwardRoundTrip(t, newTestProxy(), req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a loopback destination", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), plainForwardReasonLoopback) {
		t.Errorf("body = %q, want the refused address class named", string(body))
	}
}

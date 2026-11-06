package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// connectRoundTrip drives handleConnectDirect over a pipe for a CONNECT to
// target and returns the response the agent-side client would see.
func connectRoundTrip(t *testing.T, p *GitHubProxy, target string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodConnect, "http://"+target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = target
	req.Header.Set("Proxy-Authorization", "hive scanner")

	clientConn, proxyClient := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })

	go func() {
		p.handleConnectDirect(proxyClient, req)
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

// TestTunnelDirectRefusesNonRoutableDestination pins that the uninspected
// CONNECT path applies the same destination policy as the plain-HTTP forward:
// a CONNECT to the cloud metadata address or the RFC 1918 estate is answered
// 403 and counted against the caller, not tunnelled from the proxy's uid.
func TestTunnelDirectRefusesNonRoutableDestination(t *testing.T) {
	p := newTestProxy()
	p.proxyAdvisoryOK = true // no UID map in tests; accept the header identity

	for _, target := range []string{"169.254.169.254:80", "10.255.255.1:6443", "100.64.0.1:443"} {
		resp := connectRoundTrip(t, p, target)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s: status = %d, want 403", target, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), plainForwardAllowPrivateEnv) {
			t.Errorf("CONNECT %s: body %q should name the override env var", target, body)
		}
	}
	if got := p.violations["scanner"]; got != 3 {
		t.Errorf("violations[scanner] = %d, want 3 — each estate-bound CONNECT must be attributed and counted", got)
	}
}

// TestTunnelDirectLoopbackRefusalIsNotAViolation mirrors the plain path: a
// loopback CONNECT is a client that ignored NO_PROXY, refused but not held
// against the caller.
func TestTunnelDirectLoopbackRefusalIsNotAViolation(t *testing.T) {
	p := newTestProxy()
	p.proxyAdvisoryOK = true

	if resp := connectRoundTrip(t, p, "127.0.0.1:9"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a loopback CONNECT", resp.StatusCode)
	}
	if got := p.violations["scanner"]; got != 0 {
		t.Errorf("violations[scanner] = %d, want 0 — loopback is a misconfiguration, not a violation", got)
	}
}

// TestTunnelDirectAllowPrivateForwardLiftsGuard pins that the same boot-time
// override that lifts the plain-forward guard lifts the tunnel guard, so a
// deployment that opted in keeps working as before.
func TestTunnelDirectAllowPrivateForwardLiftsGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}()

	p := newTestProxy()
	p.allowPrivateForward = true

	if resp := connectRoundTrip(t, p, ln.Addr().String()); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the guard lifted", resp.StatusCode)
	}
}

// TestDialTunnelUpstreamRefusesNonRoutableSNI covers the transparent-TLS
// passthrough leg: the host there comes from the client's SNI, so an SNI that
// names a private address must be refused on the same evidence.
func TestDialTunnelUpstreamRefusesNonRoutableSNI(t *testing.T) {
	p := newTestProxy()

	_, err := p.dialTunnelUpstream("10.255.255.1")
	var denied *plainForwardDenied
	if !errors.As(err, &denied) {
		t.Fatalf("err = %v, want *plainForwardDenied", err)
	}
	if denied.reason != plainForwardReasonPrivate {
		t.Errorf("reason = %q, want %q", denied.reason, plainForwardReasonPrivate)
	}
}

// TestHandleTransparentTLSRefusalCountsEstateNotLoopback pins the handler-level
// split in handleTransparentTLS: a refused estate-bound SNI is counted against
// the caller, a refused loopback SNI is not. Over net.Pipe attribution yields
// "", so the counter lands under that key.
func TestHandleTransparentTLSRefusalCountsEstateNotLoopback(t *testing.T) {
	for _, tc := range []struct {
		sni  string
		want int
	}{{"10.255.255.1", 1}, {"127.0.0.1", 0}} {
		p := newTestProxy()
		clientHello := buildClientHello(tc.sni)
		clientConn, proxyConn := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.handleTransparentTLS(proxyConn, clientHello[:1])
			_ = proxyConn.Close()
		}()
		_, _ = clientConn.Write(clientHello[1:])
		<-done
		_ = clientConn.Close()
		p.mu.Lock()
		got := p.violations[""]
		p.mu.Unlock()
		if got != tc.want {
			t.Errorf("SNI %s: violations[\"\"] = %d, want %d", tc.sni, got, tc.want)
		}
	}
}

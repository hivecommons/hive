package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// testConnectProxy is a minimal CONNECT-only forward proxy that records the
// targets it was asked to tunnel to.
type testConnectProxy struct {
	ln      net.Listener
	targets atomic.Int32
	last    atomic.Value
}

func startTestConnectProxy(t *testing.T) *testConnectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cp := &testConnectProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go cp.serve(c)
		}
	}()
	return cp
}

func (cp *testConnectProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		return
	}
	cp.targets.Add(1)
	cp.last.Store(req.Host)
	up, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(up, br) }()
	_, _ = io.Copy(c, up)
}

func newChainedTestProxy(t *testing.T, cp *testConnectProxy, hosts []string) *GitHubProxy {
	t.Helper()
	sel, err := upstreamproxy.New("http://"+cp.ln.Addr().String(), hosts, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy()
	p.allowPrivateForward = true // test origins are loopback listeners
	p.SetUpstreamProxy(sel)
	return p
}

// A CONNECT passthrough for a host named by the selector must be tunnelled
// via the upstream proxy, end to end.
func TestHandleConnectDirectChainsThroughUpstreamProxy(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-proxy")
	}))
	defer origin.Close()
	originHostPort := strings.TrimPrefix(origin.URL, "https://")
	originHost, _, _ := net.SplitHostPort(originHostPort)

	cp := startTestConnectProxy(t)
	p := newChainedTestProxy(t, cp, []string{originHost})

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()
	req, _ := http.NewRequest(http.MethodConnect, "https://"+originHostPort, nil)
	req.Host = originHostPort
	go func() {
		p.handleConnectDirect(proxyClient, req)
		proxyClient.Close()
	}()

	br := bufio.NewReader(clientConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	tlsConn := tls.Client(clientConn, &tls.Config{RootCAs: pool, ServerName: originHost})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("tls handshake over tunnel: %v", err)
	}
	if _, err := io.WriteString(tlsConn, "GET / HTTP/1.1\r\nHost: "+originHost+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(tlsConn)
	if !strings.Contains(string(body), "via-proxy") {
		t.Fatalf("origin body not received through tunnel: %q", body)
	}
	if cp.targets.Load() != 1 {
		t.Fatalf("upstream proxy saw %d CONNECTs, want 1", cp.targets.Load())
	}
	if got := cp.last.Load(); got != originHostPort {
		t.Fatalf("upstream proxy CONNECT target = %v, want %s", got, originHostPort)
	}
}

// Hosts outside the selector's rules keep the direct path: the upstream proxy
// must never see them.
func TestDialUpstreamRawNonMatchingHostStaysDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte("direct"))
		_ = c.Close()
	}()

	cp := startTestConnectProxy(t)
	p := newChainedTestProxy(t, cp, []string{"ghe.example.com"})

	conn, err := p.dialUpstreamRaw(ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b, _ := io.ReadAll(conn)
	if string(b) != "direct" {
		t.Fatalf("got %q, want direct", b)
	}
	if cp.targets.Load() != 0 {
		t.Fatalf("upstream proxy saw %d CONNECTs for a non-matching host", cp.targets.Load())
	}
}

// The MITM upstream leg (dialUpstreamTLS) must complete a TLS handshake with
// the origin through the CONNECT tunnel when the host matches.
func TestDialUpstreamTLSChainsThroughUpstreamProxy(t *testing.T) {
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	origin.StartTLS()
	defer origin.Close()
	originHostPort := strings.TrimPrefix(origin.URL, "https://")
	originHost, _, _ := net.SplitHostPort(originHostPort)

	cp := startTestConnectProxy(t)
	p := newChainedTestProxy(t, cp, []string{originHost})

	conn, err := p.dialUpstreamTLS(originHostPort, originHost, 3*time.Second)
	if err == nil {
		// The handshake itself fails on cert verification against an unknown
		// CA; reaching the handshake proves the tunnel carried TLS bytes.
		conn.Close()
	}
	if cp.targets.Load() != 1 {
		t.Fatalf("upstream proxy saw %d CONNECTs, want 1 (err=%v)", cp.targets.Load(), err)
	}
	if err != nil && !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("unexpected non-TLS error through tunnel: %v", err)
	}
}

package proxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/tokens"
)

// These tests exercise handleTransparentTLS's attribution and routing branches
// (#7793): the iptables-redirect path has no CONNECT request and no
// Proxy-Authorization header, so the socket table is the only identity
// evidence, and the branch that names the hive's own control plane is the one
// that decides whether an App token mint is attributed or blocked as an
// unidentified agent. The Copilot-sniff gate and the non-inspected tunnel are
// driven end to end through a real ClientHello over loopback, with the
// upstream legs substituted by pipes so nothing leaves the host.

// procNetRow is one synthetic /proc/net/tcp row: localPort owned by uid.
type procNetRow struct {
	localPort int
	uid       int
}

// writeProcNetTable redirects the procnet parser at a synthetic IPv4 socket
// table holding exactly rows (the IPv6 table is empty).
func writeProcNetTable(t *testing.T, rows ...procNetRow) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, r := range rows {
		fmt.Fprintf(&b, "   %d: 0100007F:%04X 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  %d        0 %d 1\n",
			i, r.localPort, r.uid, i+1)
	}
	dir := t.TempDir()
	tcp4 := filepath.Join(dir, "tcp")
	tcp6 := filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcp4, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTCP, oldTCP6 := procNetTCPPath, procNetTCP6Path
	procNetTCPPath, procNetTCP6Path = tcp4, tcp6
	t.Cleanup(func() { procNetTCPPath, procNetTCP6Path = oldTCP, oldTCP6 })
}

// loopbackPair returns a connected TCP pair so the proxy-side conn has a real
// peer port to look up. The caller owns both ends.
func loopbackPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server = <-accepted:
	case err := <-errCh:
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func peerPort(t *testing.T, server net.Conn) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(server.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func newAttributionProxy(t *testing.T, uidMap *agent.UIDMap) *GitHubProxy {
	t.Helper()
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	return &GitHubProxy{
		caCert:    caCert,
		caX509:    caX509,
		uidMap:    uidMap,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		certCache: make(map[string]cachedCert),
	}
}

func TestAttributeTransparentConn(t *testing.T) {
	// The scanner agent and an unknown UID that is neither an agent nor internal.
	const unknownUID = 5555
	uidMap := &agent.UIDMap{
		Agents:   map[string]int{"scanner": agentUIDFixture},
		ProxyUID: 1001,
		BaseUID:  2001,
	}

	t.Run("no uid map means unidentified, even for a resolvable port", func(t *testing.T) {
		_, server := loopbackPair(t)
		writeProcNetTable(t, procNetRow{peerPort(t, server), agentUIDFixture})
		p := newAttributionProxy(t, nil)
		if got := p.attributeTransparentConn(server); got != "" {
			t.Fatalf("attribution without a UID map = %q, want \"\" — there is no identity evidence to trust", got)
		}
	})

	t.Run("a peer address with no port is unidentified", func(t *testing.T) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		p := newAttributionProxy(t, uidMap)
		if got := p.attributeTransparentConn(b); got != "" {
			t.Fatalf("attribution on a portless peer = %q, want \"\"", got)
		}
	})

	t.Run("the socket table's UID resolves to the agent", func(t *testing.T) {
		_, server := loopbackPair(t)
		writeProcNetTable(t, procNetRow{peerPort(t, server), agentUIDFixture})
		p := newAttributionProxy(t, uidMap)
		if got := p.attributeTransparentConn(server); got != "scanner" {
			t.Fatalf("attribution = %q, want scanner", got)
		}
	})

	t.Run("a port absent from the socket table is unidentified", func(t *testing.T) {
		_, server := loopbackPair(t)
		writeProcNetTable(t, procNetRow{peerPort(t, server) ^ 1, agentUIDFixture})
		p := newAttributionProxy(t, uidMap)
		if got := p.attributeTransparentConn(server); got != "" {
			t.Fatalf("attribution on an unlisted port = %q, want \"\"", got)
		}
	})

	t.Run("a UID owned by no agent and not internal is unidentified", func(t *testing.T) {
		_, server := loopbackPair(t)
		writeProcNetTable(t, procNetRow{peerPort(t, server), unknownUID})
		p := newAttributionProxy(t, uidMap)
		if got := p.attributeTransparentConn(server); got != "" {
			t.Fatalf("attribution on an unknown UID = %q, want \"\" — never invent an identity", got)
		}
	})

	t.Run("root and the proxy user are named as the hive's own control plane", func(t *testing.T) {
		for _, uid := range []int{0, uidMap.ProxyUID} {
			_, server := loopbackPair(t)
			writeProcNetTable(t, procNetRow{peerPort(t, server), uid})
			p := newAttributionProxy(t, uidMap)
			if got := p.attributeTransparentConn(server); got != internalCallerName {
				t.Fatalf("attribution for uid %d = %q, want %q — a hive-originated write must be attributed, not blocked as unidentified", uid, got, internalCallerName)
			}
		}
	})

	t.Run("an agent provisioned at an internal UID is the agent, never internal", func(t *testing.T) {
		// Belt-and-braces guard from UIDMap.IsInternalUID: if an agent ever
		// occupied UID 0, it must be attributed as that agent (and gated as
		// one), not handed the control plane's exemption.
		_, server := loopbackPair(t)
		writeProcNetTable(t, procNetRow{peerPort(t, server), 0})
		p := newAttributionProxy(t, &agent.UIDMap{
			Agents:   map[string]int{"rogue": 0},
			ProxyUID: 1001,
			BaseUID:  2001,
		})
		if got := p.attributeTransparentConn(server); got != "rogue" {
			t.Fatalf("attribution for an agent at uid 0 = %q, want rogue", got)
		}
	})
}

// runTransparentTLS feeds a real client-side ClientHello for sni through
// handleTransparentTLS exactly as handleConn does (1-byte peek, then the
// handler), returning the client's TLS handshake result and the proxy-side
// completion channel.
func runTransparentTLS(t *testing.T, p *GitHubProxy, sni string, clientTLS *tls.Config) (handshakeErr error, done <-chan struct{}) {
	t.Helper()
	client, server := loopbackPair(t)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		peeked := make([]byte, 1)
		if _, err := server.Read(peeked); err != nil {
			return
		}
		p.handleTransparentTLS(server, peeked)
	}()
	tlsClient := tls.Client(client, clientTLS)
	_ = tlsClient.SetDeadline(time.Now().Add(5 * time.Second))
	err := tlsClient.Handshake()
	_ = tlsClient.SetDeadline(time.Time{})
	if err == nil {
		_ = tlsClient.Close()
	} else {
		_ = client.Close()
	}
	return err, finished
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleTransparentTLS did not return")
	}
}

// TestTransparentTLSCopilotSniffGate: a Copilot completion host is MITM'd for
// token accounting ONLY when the connection is attributed to an agent and a
// token sink is wired. The proof that the sniff branch ran is the forged
// certificate: the client completes a handshake against the proxy's CA for the
// Copilot SNI, and the Copilot upstream hook (not the tunnel hook) is dialled.
func TestTransparentTLSCopilotSniffGate(t *testing.T) {
	const copilotHost = "api.githubcopilot.com"
	uidMap := &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture}, ProxyUID: 1001, BaseUID: 2001}

	// The pair is created first so the socket table can attribute its peer
	// port to the scanner.
	client, server := loopbackPair(t)
	writeProcNetTable(t, procNetRow{peerPort(t, server), agentUIDFixture})

	p := newAttributionProxy(t, uidMap)
	p.tokenSink = &tokens.InferenceSink{}
	copilotDialed := make(chan string, 1)
	p.copilotDial = func(host string) (net.Conn, error) {
		copilotDialed <- host
		return nil, errors.New("test: no upstream")
	}
	p.tunnelDial = func(host string) (net.Conn, error) {
		t.Errorf("tunnel dialled %q: an attributed Copilot connection must be sniffed, not tunnelled", host)
		return nil, errors.New("wrong path")
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		peeked := make([]byte, 1)
		if _, err := server.Read(peeked); err != nil {
			return
		}
		p.handleTransparentTLS(server, peeked)
	}()

	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)
	tlsClient := tls.Client(client, &tls.Config{ServerName: copilotHost, RootCAs: pool})
	_ = tlsClient.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client handshake against the sniff MITM failed: %v", err)
	}
	if got := tlsClient.ConnectionState().PeerCertificates[0].Subject.CommonName; got != copilotHost {
		t.Fatalf("forged cert CN = %q, want %q", got, copilotHost)
	}
	_ = tlsClient.Close()
	waitDone(t, finished)
	select {
	case host := <-copilotDialed:
		if host != copilotHost {
			t.Fatalf("copilot upstream dialled for %q, want %q", host, copilotHost)
		}
	default:
		t.Fatal("copilot upstream was never dialled — the sniff branch did not run")
	}
}

// TestTransparentTLSNonInspectedHostTunnels: a host outside the inspection set
// is tunnelled byte-for-byte — the reassembled ClientHello is replayed to the
// upstream verbatim (the upstream sees exactly what the agent sent, so its own
// TLS handshake is intact), and the agent's identity plays no part.
func TestTransparentTLSNonInspectedHostTunnels(t *testing.T) {
	const host = "example.invalid"
	if NeedsInspection(host) {
		t.Fatalf("test premise: %q must not be an inspected host", host)
	}
	p := newAttributionProxy(t, nil)

	upstreamProxySide, upstreamFar := net.Pipe()
	t.Cleanup(func() { _ = upstreamProxySide.Close(); _ = upstreamFar.Close() })
	dialed := make(chan string, 1)
	p.tunnelDial = func(h string) (net.Conn, error) {
		dialed <- h
		return upstreamProxySide, nil
	}
	p.copilotDial = func(h string) (net.Conn, error) {
		t.Errorf("copilot upstream dialled for %q on a non-Copilot host", h)
		return nil, errors.New("wrong path")
	}

	client, server := loopbackPair(t)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		peeked := make([]byte, 1)
		if _, err := server.Read(peeked); err != nil {
			return
		}
		p.handleTransparentTLS(server, peeked)
	}()

	// Drive a real ClientHello; the far end of the tunnel must receive it whole.
	clientHelloSent := make(chan struct{})
	go func() {
		defer close(clientHelloSent)
		tlsClient := tls.Client(client, &tls.Config{ServerName: host, InsecureSkipVerify: true}) //nolint:gosec // upstream is a pipe; no cert is ever presented
		_ = tlsClient.SetDeadline(time.Now().Add(5 * time.Second))
		_ = tlsClient.Handshake() // fails once we close the far end; only the flight matters
	}()

	_ = upstreamFar.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, tlsRecordHeaderLen)
	if _, err := io.ReadFull(upstreamFar, hdr); err != nil {
		t.Fatalf("upstream never received the ClientHello record header: %v", err)
	}
	const tlsHandshakeRecord = 0x16
	if hdr[0] != tlsHandshakeRecord {
		t.Fatalf("upstream got record type %#x, want handshake (0x16)", hdr[0])
	}
	body := make([]byte, int(hdr[3])<<8|int(hdr[4]))
	if _, err := io.ReadFull(upstreamFar, body); err != nil {
		t.Fatalf("upstream received a truncated ClientHello: %v", err)
	}
	if got := extractSNI(append(hdr, body...)); got != host {
		t.Fatalf("ClientHello replayed to upstream carries SNI %q, want %q — the record was not forwarded verbatim", got, host)
	}
	select {
	case h := <-dialed:
		if h != host {
			t.Fatalf("tunnel dialled %q, want %q", h, host)
		}
	default:
		t.Fatal("tunnel upstream was never dialled")
	}

	_ = upstreamFar.Close()
	_ = client.Close()
	<-clientHelloSent
	waitDone(t, finished)
}

// TestTransparentTLSTunnelFailuresClose: a tunnel whose upstream cannot be
// dialled, or whose upstream rejects the replayed ClientHello, returns without
// hanging the agent's connection.
func TestTransparentTLSTunnelFailuresClose(t *testing.T) {
	const host = "example.invalid"
	t.Run("dial failure", func(t *testing.T) {
		p := newAttributionProxy(t, nil)
		p.tunnelDial = func(string) (net.Conn, error) { return nil, errors.New("test: unreachable") }
		pool := x509.NewCertPool()
		pool.AddCert(p.caX509)
		_, done := runTransparentTLS(t, p, host, &tls.Config{ServerName: host, RootCAs: pool})
		waitDone(t, done)
	})
	t.Run("upstream closed before the replay", func(t *testing.T) {
		p := newAttributionProxy(t, nil)
		near, far := net.Pipe()
		_ = far.Close()
		p.tunnelDial = func(string) (net.Conn, error) { return near, nil }
		pool := x509.NewCertPool()
		pool.AddCert(p.caX509)
		_, done := runTransparentTLS(t, p, host, &tls.Config{ServerName: host, RootCAs: pool})
		waitDone(t, done)
	})
}

// TestTransparentTLSTruncatedClientHelloReturns: a peer that sends a record
// header and then goes quiet must not wedge the handler — the bounded
// ClientHello read gives up and the handler returns.
func TestTransparentTLSTruncatedClientHelloReturns(t *testing.T) {
	p := newAttributionProxy(t, nil)
	client, server := loopbackPair(t)
	// A handshake record header declaring 200 bytes, of which none follow, then
	// EOF: readClientHelloRecord must fail rather than block.
	if _, err := client.Write([]byte{0x16, 0x03, 0x01, 0x00, 0xC8}); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		peeked := make([]byte, 1)
		if _, err := server.Read(peeked); err != nil {
			return
		}
		p.handleTransparentTLS(server, peeked)
	}()
	waitDone(t, done)
}

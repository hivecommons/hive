package github

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Regressions for the client-side half of issue #3875: every JWT/token-mint
// call built a freshly cloned http.Transport, dialed a brand-new connection,
// and then orphaned it in a single-use idle pool nothing ever reused or
// explicitly closed — one manufactured socket per call, at the exact moment
// (a slow GitHub, a hot retry loop) the process could least afford them. The
// fix shares ONE process-wide transport, rebuilt (and its predecessor's idle
// pool reaped via CloseIdleConnections) only when the RootCAs pool changes.

// TestProxyTrustingHTTPClient_SharesTransport asserts transport identity is
// stable across mint clients while the CA is unchanged — the property that
// makes keep-alive reuse possible at all.
func TestProxyTrustingHTTPClient_SharesTransport(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	c1 := proxyTrustingHTTPClient(mintClientTimeout)
	c2 := proxyTrustingHTTPClient(mintClientTimeout)
	if c1 == c2 {
		t.Fatal("clients must be per-call wrappers (they carry per-call timeouts)")
	}
	// Each client gets its own slow-start wrapper (cheap, stateless beyond the
	// shared pacing ledger); the SOCKET-POOL guarantee (#3875) lives on the
	// wrapper's INNER transport, which must be one shared instance.
	s1, ok1 := c1.Transport.(*slowStartTransport)
	s2, ok2 := c2.Transport.(*slowStartTransport)
	if !ok1 || !ok2 {
		t.Fatalf("transport types = %T / %T, want *slowStartTransport wrappers", c1.Transport, c2.Transport)
	}
	if s1.inner != s2.inner {
		t.Fatal("mint clients must share ONE inner transport — a transport per call orphans a socket per call (#3875)")
	}
	if s1.state != s2.state {
		t.Fatal("slow-start pacing state must be shared — per-client state would let the retry herd stampede in aggregate")
	}
}

// TestProxyTrustingHTTPClient_RebuildsTransportOnCAChange asserts the shared
// transport is invalidated when the proxy CA rotates (pool rebuild), so a
// fresh-PVC boot still picks up the CA written after process start — the
// guarantee the old per-call construction provided implicitly.
func TestProxyTrustingHTTPClient_RebuildsTransportOnCAChange(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	withProxyCAPath(t, caPath)

	before := sharedProxyTrust.sharedTransport() // CA absent: system-only trust

	writeTestCA(t, caPath)
	sharedProxyTrust.mu.Lock()
	sharedProxyTrust.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	sharedProxyTrust.mu.Unlock()

	after := sharedProxyTrust.sharedTransport()
	if before == after {
		t.Fatal("transport must be rebuilt when the proxy CA appears — otherwise the mint never trusts a CA written after start")
	}
	if after.TLSClientConfig == nil || after.TLSClientConfig.RootCAs == nil {
		t.Fatal("rebuilt transport must carry the (system + proxy CA) RootCAs pool")
	}
	// And stay stable once rebuilt.
	if again := sharedProxyTrust.sharedTransport(); again != after {
		t.Fatal("transport must be stable while the CA is unchanged")
	}
}

// TestProxyTrustingHTTPClient_ReusesConnections is the leak-count regression:
// N sequential mint-style calls (a fresh client wrapper per call, exactly as
// newJWTClient/newTokenClient construct them) against one TLS server must
// ride a small number of underlying connections. Pre-fix this was one NEW
// connection per call — the per-call socket manufacturing measured live as
// part of the #3875 FD pile.
func TestProxyTrustingHTTPClient_ReusesConnections(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	withProxyCAPath(t, caPath)

	var connsOpened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connsOpened.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	// Trust the test server exactly the way production trusts the MITM proxy:
	// its cert PEM at proxyCAPath (self-signed, so it is its own root).
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o644); err != nil {
		t.Fatalf("write server cert as proxy CA: %v", err)
	}
	if _, err := x509.ParseCertificate(srv.Certificate().Raw); err != nil {
		t.Fatalf("parse server cert: %v", err)
	}

	const calls = 6
	for i := 0; i < calls; i++ {
		client := proxyTrustingHTTPClient(mintClientTimeout) // fresh wrapper per call, like every mint
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Keep-alive reuse must hold: allow a little slack for a mid-run
	// CA-pool/transport settle, but nothing close to one conn per call.
	const maxConns = 2
	if got := connsOpened.Load(); got > maxConns {
		t.Fatalf("%d calls opened %d connections (want <= %d) — per-call transports are manufacturing sockets again (#3875)", calls, got, maxConns)
	}
}

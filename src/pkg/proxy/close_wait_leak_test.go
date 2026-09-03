package proxy

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// Regressions for issue #3875: the hive process leaked CLOSE_WAIT sockets to
// GitHub through its own MITM proxy until FD exhaustion (92,962 FDs measured
// live) self-DoSed the pod. #3872 bounded relayTunnel's half-close drains, but
// two proxyHTTP paths kept the pre-#3872 shape:
//
//  1. the git smart-HTTP branch ran its OWN unbounded io.Copy pair, so an
//     upstream FIN with a half-open client parked the handler at <-done
//     forever — the deferred conn Closes never ran and the FIN'd upstream
//     socket sat in CLOSE_WAIT for the life of the process;
//  2. the API branch cleared the upstream read deadline once response HEADERS
//     arrived, so a "reachable but slow" GitHub (TLS + headers OK, body never
//     comes — the measured live signature) parked resp.Write in an unbounded
//     upstream.Read holding all three sockets of the exchange, one fresh
//     parked handler per retry (~1k FDs/min measured on a live spoke).
//
// The leak invariant in both: proxyHTTP MUST RETURN once the exchange can no
// longer make progress, because the sockets are closed by the CALLERS'
// deferred Closes — a handler that never returns is a socket that never
// closes.

func leakTestProxy() *GitHubProxy {
	return &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func shortenBodyStall(t *testing.T, d time.Duration) {
	t.Helper()
	old := responseBodyStallTimeout
	responseBodyStallTimeout = d
	t.Cleanup(func() { responseBodyStallTimeout = old })
}

func runProxyHTTP(p *GitHubProxy, client, upstream net.Conn) chan struct{} {
	returned := make(chan struct{})
	go func() {
		p.proxyHTTP(client, upstream, internalCallerName, 0, agent.AgentCapabilities{})
		close(returned)
	}()
	return returned
}

// TestProxyHTTPGitRelayReleasesAfterUpstreamFIN is the regression for leak
// (1): upstream sends the git response and FINs; the client keeps its side
// half-open (keep-alive). Pre-fix the inline copy pair blocked in
// client.Read() forever and <-done never returned; the FIN'd upstream fd was
// CLOSE_WAIT until process death. Post-fix the git branch goes through
// relayTunnel, whose half-close drain releases the handler.
func TestProxyHTTPGitRelayReleasesAfterUpstreamFIN(t *testing.T) {
	shortenTunnelDrain(t, 300*time.Millisecond)
	p := leakTestProxy()
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)

	returned := runProxyHTTP(p, proxyClientSide, proxyUpstreamSide)

	req := "GET /hivecommons/hive.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\n\r\n"
	if _, err := agentSide.Write([]byte(req)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 4096)
	forgeSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := forgeSide.Read(buf); err != nil {
		t.Fatalf("upstream did not receive relayed git request: %v", err)
	}
	const gitResp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	if _, err := forgeSide.Write([]byte(gitResp)); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	forgeSide.Close() // remote FIN — our side must eventually close too

	// Positive control: the response reached the client through the relay.
	respBuf := make([]byte, len(gitResp))
	agentSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(agentSide, respBuf); err != nil {
		t.Fatalf("client did not receive relayed git response: %v", err)
	}
	// The client deliberately does NOT close: half-open keep-alive is the
	// leak-triggering state.

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP wedged after upstream FIN with a half-open client — the CLOSE_WAIT leak (#3875)")
	}
}

// TestProxyHTTPBodyStallReleasesHandler is the regression for leak (2):
// headers arrive promptly, the body never does, and the client gives up and
// closes (the 10s collect budget firing). Pre-fix resp.Write sat in an
// unbounded upstream.Read forever. Post-fix the rolling stall bound errors the
// relay out and proxyHTTP returns.
func TestProxyHTTPBodyStallReleasesHandler(t *testing.T) {
	shortenBodyStall(t, 300*time.Millisecond)
	p := leakTestProxy()
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)

	returned := runProxyHTTP(p, proxyClientSide, proxyUpstreamSide)

	req := "GET /app/installations/1/access_tokens HTTP/1.1\r\nHost: api.github.com\r\n\r\n"
	if _, err := agentSide.Write([]byte(req)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 4096)
	forgeSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := forgeSide.Read(buf); err != nil {
		t.Fatalf("upstream did not receive relayed request: %v", err)
	}
	// Headers promptly; the 1000-byte body NEVER — GitHub's measured
	// "reachable but slow" signature (TLS in 0.03–2.2s, 0 body bytes in 12s).
	if _, err := forgeSide.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n")); err != nil {
		t.Fatalf("upstream header write: %v", err)
	}
	// The minting client's context deadline fires; it hangs up.
	time.Sleep(100 * time.Millisecond)
	agentSide.Close()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP wedged on a stalled response body — the parked-handler FD leak (#3875)")
	}
}

// TestProxyHTTPBodyRelayToleratesSlowButFlowingBody is the positive control
// against over-fixing: the stall bound must be a rolling PER-READ bound, not a
// cap on total transfer time. A body that keeps trickling — each gap well
// under the stall bound, total time well over it — must arrive intact.
func TestProxyHTTPBodyRelayToleratesSlowButFlowingBody(t *testing.T) {
	shortenBodyStall(t, 500*time.Millisecond)
	p := leakTestProxy()
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)

	returned := runProxyHTTP(p, proxyClientSide, proxyUpstreamSide)

	req := "GET /repos/hivecommons/hive/issues HTTP/1.1\r\nHost: api.github.com\r\n\r\n"
	if _, err := agentSide.Write([]byte(req)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 4096)
	forgeSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := forgeSide.Read(buf); err != nil {
		t.Fatalf("upstream did not receive relayed request: %v", err)
	}
	const chunks = 6
	body := strings.Repeat("x", chunks)
	if _, err := forgeSide.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\n")); err != nil {
		t.Fatalf("upstream header write: %v", err)
	}
	// 6 bytes over ~900ms: every inter-byte gap (150ms) is far inside the
	// 500ms stall bound, but the TOTAL exceeds it. An absolute deadline would
	// cut this transfer; the rolling bound must not.
	for i := 0; i < chunks; i++ {
		time.Sleep(150 * time.Millisecond)
		if _, err := forgeSide.Write([]byte{body[i]}); err != nil {
			t.Fatalf("upstream body write %d: %v", i, err)
		}
	}

	respBuf := make([]byte, 512)
	agentSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := ""
	for !strings.HasSuffix(got, body) {
		n, err := agentSide.Read(respBuf)
		if err != nil {
			t.Fatalf("client read: %v (got so far: %q)", err, got)
		}
		got += string(respBuf[:n])
	}

	agentSide.Close()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP did not return after the client closed")
	}
}

// TestProxyHTTPHasNoInlineCopyPairs asserts the source-level invariant behind
// leak (1): every opaque byte-shuttling path in github_proxy.go must go
// through relayTunnel (whose half-close drains are the #3872 guarantee), never
// a hand-rolled io.Copy pair. The pre-fix git branch was exactly such a pair
// that #3872 missed. relayTunnel's own two copies are the only sanctioned
// ones.
func TestProxyHTTPHasNoInlineCopyPairs(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "github_proxy.go"))
	if err != nil {
		t.Fatalf("read github_proxy.go: %v", err)
	}
	text := string(body)
	const wantRelayTunnelCalls = 3 // transparent passthrough, CONNECT tunnelDirect, git branch
	if got := strings.Count(text, "relayTunnel(") - strings.Count(text, "func relayTunnel("); got != wantRelayTunnelCalls {
		t.Fatalf("relayTunnel call sites = %d, want %d — opaque relays must use relayTunnel, not inline io.Copy pairs", got, wantRelayTunnelCalls)
	}
	// relayTunnel's own two directional copies are the only raw conn-to-conn
	// io.Copy allowed in this file.
	if got := strings.Count(text, "io.Copy(upstream, client)"); got != 0 {
		t.Fatalf("found %d inline io.Copy(upstream, client) — the unbounded copy-pair shape that leaked CLOSE_WAIT sockets (#3875)", got)
	}
	// And the API branch must bound its body relay.
	if !strings.Contains(text, "resp.Body = &stallBoundedBody{") {
		t.Fatal("proxyHTTP response body relay must be wrapped in stallBoundedBody — an unbounded body read parks the handler and leaks its sockets (#3875)")
	}
}

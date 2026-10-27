package proxy

import (
	"io"
	"net"
	"sync/atomic"
	"time"
)

// responseBodyStallTimeout bounds how long the MITM'd response-BODY relay may
// go without a single byte of progress from the upstream. It is a rolling
// per-Read bound (re-armed on every read by stallBoundedBody), not a cap on
// total transfer time, so a large-but-flowing response is never cut while an
// upstream that goes silent mid-body releases the handler — and with it the
// three sockets of the exchange — within one stall window. Overridable, not a
// const, so tests can shorten it — and atomic for the same reason as
// tunnelHalfCloseDrain (#5553): the reading goroutine can outlive the test
// that started it. A handler test that returns after a fixed sleep leaves
// proxyHTTP still parsing the response when the NEXT test's shortenBodyStall
// writes the bound — a plain var made that an unsynchronised write-after-read
// (-race failure on v4, #6110, reported against whichever test held the
// baton). Defaults to httpReadTimeout: the same patience already extended to
// the header phase.
var responseBodyStallTimeout atomic.Int64

func init() { responseBodyStallTimeout.Store(int64(httpReadTimeout)) }

// bodyStallTimeout returns the current response-body stall bound.
func bodyStallTimeout() time.Duration {
	return time.Duration(responseBodyStallTimeout.Load())
}

// bodyStallTimeoutDuration returns the body-stall timeout for this proxy instance,
// falling back to the package-level responseBodyStallTimeout.
func (p *GitHubProxy) bodyStallTimeoutDuration() time.Duration {
	if p != nil && p.bodyStallTimeout > 0 {
		return p.bodyStallTimeout
	}
	return bodyStallTimeout()
}

// stallBoundedBody wraps a proxied response body so every underlying Read is
// preceded by re-arming a read deadline on the upstream conn. Close clears the
// deadline (for the next keep-alive exchange on the same conn) and closes the
// wrapped body. See responseBodyStallTimeout for why this exists (#3875).
type stallBoundedBody struct {
	body io.ReadCloser
	conn net.Conn
	idle time.Duration
}

func (b *stallBoundedBody) Read(p []byte) (int, error) {
	_ = b.conn.SetReadDeadline(time.Now().Add(b.idle))
	return b.body.Read(p)
}

func (b *stallBoundedBody) Close() error {
	// Close the wrapped body while a stall deadline is armed: net/http body
	// Close paths may still read from the conn (drain-to-EOF), and an unarmed
	// read there would reintroduce the very stall this type exists to bound.
	// The deadline is cleared afterwards so the next keep-alive exchange on
	// this conn starts unencumbered.
	_ = b.conn.SetReadDeadline(time.Now().Add(b.idle))
	err := b.body.Close()
	_ = b.conn.SetReadDeadline(time.Time{})
	return err
}

// stallBoundedWriter is the write-side mirror of stallBoundedBody: every Write
// re-arms the client's write deadline, so the bound is "no progress for idle"
// rather than "the whole relay must finish within idle".
//
// The distinction is not academic. The response relays used to arm a single
// absolute client.SetWriteDeadline(now+httpWriteTimeout) around
// resp.Write(client), which is correct for a small buffered body and wrong for
// a stream: the MITM'd /v1/messages SSE relay holds one Write loop open for the
// entire generation, so a turn that legitimately streams for longer than
// httpWriteTimeout was severed mid-stream at exactly that mark — even though
// tokens were flowing the whole time. The agent saw a truncated body
// ("Response was interrupted due to a server error", or a TLS-level "cannot
// decrypt peer's message" when the cut landed mid-record), retried, and —
// because a retry replays the same long turn — was cut again at the same 60s,
// livelocking the agent on its longest and most valuable turns. Measured on the
// hosted spoke: 23 "response relay timed out" in 40 minutes, every one
// path=/v1/messages with error "write tcp ...: i/o timeout", against an agent
// that made zero forward progress across six consecutive attempts.
//
// Reusing the stall semantics already proven on the read side (#3875) keeps a
// client that has genuinely gone away bounded — a dead peer stops draining, the
// socket buffer fills, and the next Write blocks past idle and errors — while a
// stream that keeps flowing is never cut for being long.
type stallBoundedWriter struct {
	conn net.Conn
	idle time.Duration
}

func (w *stallBoundedWriter) Write(p []byte) (int, error) {
	_ = w.conn.SetWriteDeadline(time.Now().Add(w.idle))
	return w.conn.Write(p)
}

// tunnelHalfCloseDrainDefault bounds how long one direction of an opaque
// tunnel may keep waiting for bytes AFTER the other direction has already
// finished.
//
// 30s is deliberately generous: the only legitimate traffic still in flight
// once one side has hung up is a response tail already on the wire, which
// arrives in RTTs, not tens of seconds.
const tunnelHalfCloseDrainDefault = 30 * time.Second

// tunnelHalfCloseDrain is overridable rather than const for the same reason as
// waitForReadyPollInterval in pkg/hub: tests need a relay that unwedges in
// milliseconds. Production keeps the default.
//
// It is atomic because the write and the read genuinely happen on different
// goroutines with no happens-before between them. relayTunnel reads the bound
// from the copy goroutine it spawns, and that goroutine can outlive the test
// that started it: a handler test only waits for the CONNECT response, not for
// the relay to drain, so the goroutine is still reading while the NEXT test's
// shortenTunnelDrain writes. A plain var made that an unsynchronised
// write-after-read and -race failed whichever test happened to hold the baton
// (#5553) — the reported test name was incidental, which is why the failure
// moved between TestRelayTunnelUnwedgesFromSilentUpstream and
// TestRelayTunnelDataFlowsBothWays across shuffle seeds.
var tunnelHalfCloseDrain atomic.Int64

func init() { tunnelHalfCloseDrain.Store(int64(tunnelHalfCloseDrainDefault)) }

// tunnelDrain returns the current half-close drain bound.
func tunnelDrain() time.Duration {
	return time.Duration(tunnelHalfCloseDrain.Load())
}

// relayTunnel shuttles bytes both ways between the proxied client conn and the
// upstream until BOTH directions have finished, bounding each direction once
// the other has ended.
//
// WHY THE BOUNDS EXIST (measured live, 2026-08-14): the previous inline copies
// waited on each other unboundedly. On spokes whose forge host
// (github.ibm.com) sits behind a SYN-accepting firewall, the upstream TCP
// connect succeeds but no byte ever comes back and no FIN is ever sent. The
// client (the daemon's own JWT mints, agents' CLIs) gives up at its TLS
// handshake timeout and closes — the client→upstream copy finishes and
// half-closes the upstream — but io.Copy(conn, upstream) stayed blocked in
// upstream.Read() FOREVER. Every attempt leaked two sockets (the FIN_WAIT2 /
// CLOSE_WAIT piles), the goroutine, and the splice pipe pair io.Copy holds for
// a TCP↔TCP copy on Linux: ~300k fds and ~11 burned cores per affected spoke.
//
// SetReadDeadline is documented to interrupt Reads that are ALREADY blocked,
// which is exactly what arms the escape hatch here: each side arms the OTHER
// side's deadline only at the moment its own direction completes, so a
// healthy long-lived tunnel (git smart HTTP, device-flow polling) is never
// cut while both directions are still live.
func relayTunnel(conn, upstream net.Conn) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		if tc, ok := upstream.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		// Client side is done (EOF or error — a dead client looks the same).
		// A live upstream answers or closes within RTTs; a blackholed one
		// never would, so bound the remaining upstream→client read.
		_ = upstream.SetReadDeadline(time.Now().Add(tunnelDrain()))
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	// Mirror image: upstream finished (or errored) but the client may sit
	// half-open without ever sending EOF; bound the client-side read so
	// <-done cannot wedge this handler.
	_ = conn.SetReadDeadline(time.Now().Add(tunnelDrain()))
	<-done
}

func transfer(dst, src net.Conn) {
	defer func() { _ = dst.Close() }()
	defer func() { _ = src.Close() }()
	_, _ = io.Copy(dst, src)
}

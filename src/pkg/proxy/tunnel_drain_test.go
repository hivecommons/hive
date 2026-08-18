package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns two ends of a real localhost TCP connection. relayTunnel's
// escape hatch relies on net.Conn deadline semantics interrupting an
// in-flight blocked Read, so the test must use real sockets, not net.Pipe.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)
	go func() {
		c, aerr := ln.Accept()
		ch <- acceptResult{c, aerr}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatalf("accept: %v", res.err)
	}
	t.Cleanup(func() { client.Close(); res.conn.Close() })
	return client, res.conn
}

func shortenTunnelDrain(t *testing.T, d time.Duration) {
	t.Helper()
	old := tunnelHalfCloseDrain
	tunnelHalfCloseDrain = d
	t.Cleanup(func() { tunnelHalfCloseDrain = old })
}

func runRelay(conn, upstream net.Conn) chan struct{} {
	returned := make(chan struct{})
	go func() {
		relayTunnel(conn, upstream)
		close(returned)
	}()
	return returned
}

// TestRelayTunnelDataFlowsBothWays is the positive control: the drain bounds
// must not have broken the relay itself. Bytes written on either side while
// both directions are live must arrive intact on the other.
func TestRelayTunnelDataFlowsBothWays(t *testing.T) {
	shortenTunnelDrain(t, 2*time.Second)
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)

	returned := runRelay(proxyClientSide, proxyUpstreamSide)

	if _, err := agentSide.Write([]byte("ping")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	buf := make([]byte, 4)
	forgeSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(forgeSide, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("client→upstream relay: got %q err %v", buf, err)
	}
	if _, err := forgeSide.Write([]byte("pong")); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	agentSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(agentSide, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("upstream→client relay: got %q err %v", buf, err)
	}

	// Orderly shutdown from both ends releases the relay.
	agentSide.Close()
	forgeSide.Close()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("relayTunnel did not return after both ends closed")
	}
}

// TestRelayTunnelUnwedgesFromSilentUpstream is the regression for the live
// incident: the upstream TCP-connects (SYN-accepting firewall in front of an
// unreachable forge) but never sends a byte and never FINs. The client gives
// up and closes. Before the drain bound, io.Copy(conn, upstream) stayed
// blocked in upstream.Read() forever, leaking both sockets, the goroutine and
// io.Copy's splice pipe pair on every attempt.
func TestRelayTunnelUnwedgesFromSilentUpstream(t *testing.T) {
	shortenTunnelDrain(t, 300*time.Millisecond)
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)
	_ = forgeSide // held open, silent: never reads, never writes, never closes

	returned := runRelay(proxyClientSide, proxyUpstreamSide)

	if _, err := agentSide.Write([]byte("client-hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	agentSide.Close() // client-side TLS timeout fires; the client is gone

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("relayTunnel wedged on a silent upstream after the client closed — the leak this fix exists for")
	}
}

// TestRelayTunnelUnwedgesFromSilentClient is the mirror image: the upstream
// closes but the client sits half-open forever without sending EOF. <-done
// must not wedge the handler.
func TestRelayTunnelUnwedgesFromSilentClient(t *testing.T) {
	shortenTunnelDrain(t, 300*time.Millisecond)
	agentSide, proxyClientSide := tcpPair(t)
	proxyUpstreamSide, forgeSide := tcpPair(t)
	_ = agentSide // held open, silent

	returned := runRelay(proxyClientSide, proxyUpstreamSide)

	if _, err := forgeSide.Write([]byte("gone")); err != nil {
		t.Fatalf("upstream write: %v", err)
	}
	forgeSide.Close()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("relayTunnel wedged on a silent client after the upstream closed")
	}
}

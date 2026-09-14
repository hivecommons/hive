package proxy

import (
	"net"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// readClientHelloRecord error and cap paths.
//
// The happy fragmented-reassembly path is pinned by
// proxy_transparent_tls_repro_test.go; these tests pin the remaining
// branches: a connection that dies before ANY bytes arrive, a ClientHello
// whose declared record length exceeds the buffer cap, and a record body
// truncated by connection close (which must return the partial bytes, not an
// error, so extractSNI can degrade gracefully).
// ─────────────────────────────────────────────────────────────────────────────

// TestReadClientHelloRecord_ImmediateCloseNoBytes: nothing peeked and the
// connection closes before a single byte — the error must propagate (there is
// nothing for extractSNI to degrade onto).
func TestReadClientHelloRecord_ImmediateCloseNoBytes(t *testing.T) {
	client, server := net.Pipe()
	client.Close()
	server.SetReadDeadline(time.Now().Add(2 * time.Second))

	got, err := readClientHelloRecord(server, nil)
	if err == nil {
		t.Fatalf("want error for connection closed with zero bytes, got %d bytes", len(got))
	}
	if got != nil {
		t.Fatalf("want nil buffer with error, got %d bytes", len(got))
	}
}

// TestReadClientHelloRecord_OversizedRecordCappedAtBuffer: a record header
// declaring a length beyond tlsClientHelloMaxSize must cap the read at the
// buffer size instead of overrunning it — the remainder is tls.Server's
// problem via prefixConn; this function only needs enough for SNI.
func TestReadClientHelloRecord_OversizedRecordCappedAtBuffer(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Header declares a 0xFFFF-byte record: header+body far exceeds the cap.
	header := []byte{0x16, 0x03, 0x01, 0xFF, 0xFF}
	go func() {
		filler := make([]byte, tlsClientHelloMaxSize) // more than the cap needs
		client.Write(filler)
	}()

	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readClientHelloRecord(server, header)
	if err != nil {
		t.Fatalf("readClientHelloRecord: %v", err)
	}
	if len(got) != tlsClientHelloMaxSize {
		t.Fatalf("oversized record read %d bytes, want buffer cap %d", len(got), tlsClientHelloMaxSize)
	}
}

// TestReadClientHelloRecord_TruncatedBodyReturnsPartial: the peer closes
// mid-record. The partial buffer must come back with a nil error so extractSNI
// can degrade gracefully on the short read.
func TestReadClientHelloRecord_TruncatedBodyReturnsPartial(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()

	// Header declares 100 body bytes; only 10 ever arrive.
	header := []byte{0x16, 0x03, 0x01, 0x00, 0x64}
	go func() {
		client.Write(make([]byte, 10))
		client.Close()
	}()

	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readClientHelloRecord(server, header)
	if err != nil {
		t.Fatalf("want nil error on truncated body (graceful degrade), got %v", err)
	}
	if want := len(header) + 10; len(got) != want {
		t.Fatalf("truncated read = %d bytes, want %d", len(got), want)
	}
}

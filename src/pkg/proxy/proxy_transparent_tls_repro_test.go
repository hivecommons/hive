package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// fragmentConn wraps a net.Conn and caps how many bytes each Read returns, to
// simulate TCP delivering a client flight split across multiple segments. TCP
// guarantees nothing about record boundaries: a single Read on the socket may
// return only part of what the peer wrote. The transparent-TLS peek used a
// single Read and so mis-framed a ClientHello that did not arrive whole,
// choosing the wrong host (SNI missing from the truncated record) and forging a
// cert the agent rejects — the agent-side handshake then fails deterministically
// (observed as "bad certificate" / "bad record MAC"), which is why every Copilot
// agent could not authenticate.
type fragmentConn struct {
	net.Conn
	max int
}

func (c *fragmentConn) Read(b []byte) (int, error) {
	if c.max > 0 && len(b) > c.max {
		b = b[:c.max]
	}
	return c.Conn.Read(b)
}

// TestTransparentTLSPartialClientHello reproduces (before the fix) and guards
// (after the fix) the transparent-TLS failure that blocked Copilot login: the
// agent's ClientHello arrives split across TCP reads, so the proxy's peek
// captured only PART of it. It drives the real peek/replay path
// (readClientHelloRecord -> extractSNI -> forgeCert(sni) -> tls.Server over
// prefixConn), exactly as handleTransparentTLS does, while forcing every Read on
// the client socket to return at most a few bytes.
//
// With the old single-Read peek this failed: extractSNI saw a truncated
// ClientHello, missed the SNI, host fell back to "github.com", the forged cert
// did not match the agent's requested "api.github.com", and the handshake failed
// with "bad certificate". readClientHelloRecord reassembles the full ClientHello
// first, so the correct cert is forged and the handshake succeeds regardless of
// fragmentation.
func TestTransparentTLSPartialClientHello(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	p := &GitHubProxy{
		caCert:    caCert,
		caX509:    caX509,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		certCache: make(map[string]cachedCert),
	}

	// fragmentSize=5 splits the ClientHello (hundreds of bytes) across many
	// reads, guaranteeing the first read is partial — the exact TCP condition the
	// single-Read peek mishandled.
	const fragmentSize = 5
	const wantSNI = "api.github.com"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type result struct {
		sni string
		err error
	}
	srvCh := make(chan result, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			srvCh <- result{"", err}
			return
		}
		defer raw.Close()
		conn := net.Conn(&fragmentConn{Conn: raw, max: fragmentSize})

		// handleConn's 1-byte peek.
		peeked := make([]byte, 1)
		if _, err := conn.Read(peeked); err != nil {
			srvCh <- result{"", err}
			return
		}
		// handleTransparentTLS's ClientHello read + SNI + forge + replay.
		fullBuf, err := readClientHelloRecord(conn, peeked)
		if err != nil {
			srvCh <- result{"", err}
			return
		}
		sni := extractSNI(fullBuf)
		if sni == "" {
			sni = "github.com" // same fallback as production
		}
		cert, err := p.forgeCert(sni)
		if err != nil {
			srvCh <- result{sni, err}
			return
		}
		srv := tls.Server(&prefixConn{Conn: conn, prefix: fullBuf}, &tls.Config{
			Certificates: []tls.Certificate{cert},
		})
		srv.SetDeadline(time.Now().Add(5 * time.Second))
		srvCh <- result{sni, srv.Handshake()}
	}()

	pool := x509.NewCertPool()
	pool.AddCert(caX509)
	clientConn, clientErr := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName: wantSNI,
		RootCAs:    pool,
	})
	if clientConn != nil {
		defer clientConn.Close()
	}

	got := <-srvCh

	if got.sni != wantSNI {
		t.Errorf("SNI extracted from fragmented ClientHello = %q, want %q (peek did not reassemble the full ClientHello)", got.sni, wantSNI)
	}
	if got.err != nil {
		t.Fatalf("server-side handshake failed on fragmented ClientHello: %v (client err: %v)", got.err, clientErr)
	}
	if clientErr != nil {
		t.Fatalf("client-side handshake failed on fragmented ClientHello: %v", clientErr)
	}
}

// TestReadClientHelloRecordReassembles is a focused unit test on the helper: a
// ClientHello delivered in tiny fragments must be reassembled into the complete
// record so its full length is present for SNI extraction.
func TestReadClientHelloRecordReassembles(t *testing.T) {
	// Build a minimal well-formed TLS record: header (type=0x16, version, len)
	// plus `body` bytes. The helper must return header+body in full even when
	// each Read yields only 1 byte.
	body := make([]byte, 300)
	for i := range body {
		body[i] = byte(i)
	}
	rec := make([]byte, tlsRecordHeaderLen+len(body))
	rec[0] = 0x16 // handshake
	rec[1] = 0x03
	rec[2] = 0x01
	rec[3] = byte(len(body) >> 8)
	rec[4] = byte(len(body))
	copy(rec[tlsRecordHeaderLen:], body)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go func() {
		// Write one byte at a time to force maximal fragmentation.
		for _, b := range rec {
			if _, err := client.Write([]byte{b}); err != nil {
				return
			}
		}
	}()

	// Consume the 1-byte peek first (as handleConn does).
	peeked := make([]byte, 1)
	if _, err := server.Read(peeked); err != nil {
		t.Fatal(err)
	}
	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readClientHelloRecord(server, peeked)
	if err != nil {
		t.Fatalf("readClientHelloRecord: %v", err)
	}
	if len(got) != len(rec) {
		t.Fatalf("reassembled length = %d, want %d (record not fully reassembled)", len(got), len(rec))
	}
	for i := range rec {
		if got[i] != rec[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], rec[i])
		}
	}
}

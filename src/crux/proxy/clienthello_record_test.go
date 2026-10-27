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

func TestReadClientHelloRecordErrorAndCapPaths(t *testing.T) {
	cases := []struct {
		name     string
		peeked   []byte
		write    func(*testing.T, net.Conn)
		wantLen  int
		wantErr  bool
		wantNil  bool
		contract string
	}{
		{
			name:    "immediate close before any bytes returns error",
			wantErr: true,
			wantNil: true,
			write: func(t *testing.T, client net.Conn) {
				t.Helper()
				if err := client.Close(); err != nil {
					t.Fatalf("close client: %v", err)
				}
			},
			contract: "a zero-byte close leaves no ClientHello prefix to replay or inspect, so the read error must not be hidden",
		},
		{
			name:    "oversized record is capped at inspection buffer",
			peeked:  []byte{0x16, 0x03, 0x01, 0xFF, 0xFF},
			wantLen: tlsClientHelloMaxSize,
			write: func(t *testing.T, client net.Conn) {
				t.Helper()
				go func() {
					_, _ = client.Write(make([]byte, tlsClientHelloMaxSize))
					_ = client.Close()
				}()
			},
			contract: "a hostile length field must not make the proxy buffer past tlsClientHelloMaxSize",
		},
		{
			name:    "truncated body returns partial buffer without error",
			peeked:  []byte{0x16, 0x03, 0x01, 0x00, 0x64},
			wantLen: tlsRecordHeaderLen + 10,
			write: func(t *testing.T, client net.Conn) {
				t.Helper()
				go func() {
					_, _ = client.Write(make([]byte, 10))
					_ = client.Close()
				}()
			},
			contract: "a short ClientHello still has to be replayed so extractSNI/tls.Server can fail gracefully instead of dropping buffered bytes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			defer client.Close()

			tc.write(t, client)
			server.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, err := readClientHelloRecord(server, tc.peeked)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s: want error, got %d bytes", tc.contract, len(got))
				}
			} else if err != nil {
				t.Fatalf("%s: readClientHelloRecord returned error: %v", tc.contract, err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("%s: want nil buffer, got %d bytes", tc.contract, len(got))
				}
				return
			}
			if len(got) != tc.wantLen {
				t.Fatalf("%s: got %d bytes, want %d", tc.contract, len(got), tc.wantLen)
			}
		})
	}
}

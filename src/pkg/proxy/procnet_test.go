package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

// Sample /proc/net/tcp content (simplified but structurally correct).
// Fields: sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode
const sampleProcNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:4803 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1001        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:C350 0100007F:4803 01 00000000:00000000 00:00000000 00000000  2001        0 12346 1 0000000000000000 100 0 0 10 0
   2: 0100007F:C351 0100007F:4803 01 00000000:00000000 00:00000000 00000000  2002        0 12347 1 0000000000000000 100 0 0 10 0
   3: 00000000:0BBB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  2003        0 12348 1 0000000000000000 100 0 0 10 0
`

func TestLookupUIDByLocalPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tcp")
	if err := os.WriteFile(path, []byte(sampleProcNetTCP), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		port    int
		wantUID int
		wantErr bool
	}{
		{name: "proxy port 0x4803=18435", port: 0x4803, wantUID: 1001},
		{name: "agent on port 0xC350=50000", port: 0xC350, wantUID: 2001},
		{name: "agent on port 0xC351=50001", port: 0xC351, wantUID: 2002},
		{name: "listening socket 0x0BBB=3003", port: 0x0BBB, wantUID: 2003},
		{name: "unknown port", port: 9999, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, err := lookupUIDByLocalPortFrom(path, tt.port)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got uid=%d", uid)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if uid != tt.wantUID {
				t.Errorf("got uid=%d, want %d", uid, tt.wantUID)
			}
		})
	}
}

func TestLookupUID_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tcp")
	os.WriteFile(path, []byte(""), 0o644)

	_, err := lookupUIDByLocalPortFrom(path, 80)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestLookupUID_MissingFile(t *testing.T) {
	_, err := lookupUIDByLocalPortFrom("/nonexistent/tcp", 80)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// Sample /proc/net/tcp6 content: same column layout as /proc/net/tcp but the
// local_address is a 32-hex IPv6 address. This is where a "127.0.0.1"/localhost
// proxy connection can land (as ::1 or ::ffff:127.0.0.1), so the UID lookup must
// scan it too.
const sampleProcNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:0BBA 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2006        0 99999 1 0000000000000000 100 0 0 10 0
`

// TestLookupUIDByLocalPort_TCP6Fallback verifies a socket that appears ONLY in
// /proc/net/tcp6 (not the IPv4 table) is still resolved. This is the spyre bug:
// an agent's GitHub API connection over IPv6 loopback was missed, so the proxy
// saw no UID, treated the agent as unidentified (advisory), and blocked its
// POST /pulls even though the agent was in ISSUES_AND_PRS mode.
func TestLookupUIDByLocalPort_TCP6Fallback(t *testing.T) {
	dir := t.TempDir()
	tcp4 := filepath.Join(dir, "tcp")
	tcp6 := filepath.Join(dir, "tcp6")
	// IPv4 table has only an UNRELATED socket, so the target port is absent there.
	if err := os.WriteFile(tcp4, []byte(sampleProcNetTCP), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte(sampleProcNetTCP6), 0o644); err != nil {
		t.Fatal(err)
	}

	oldTCP, oldTCP6 := procNetTCPPath, procNetTCP6Path
	procNetTCPPath, procNetTCP6Path = tcp4, tcp6
	t.Cleanup(func() { procNetTCPPath, procNetTCP6Path = oldTCP, oldTCP6 })

	// Port 0x0BBA=3002 exists only in the tcp6 table -> must be found via fallback.
	uid, err := LookupUIDByLocalPort(0x0BBA)
	if err != nil {
		t.Fatalf("tcp6 socket should be found via fallback: %v", err)
	}
	if uid != 2006 {
		t.Errorf("got uid=%d, want 2006 (from tcp6 table)", uid)
	}

	// A port in neither table still errors.
	if _, err := LookupUIDByLocalPort(0x9999); err == nil {
		t.Error("expected error for port in neither table")
	}

	// A port in the IPv4 table is still found (no regression).
	if uid, err := LookupUIDByLocalPort(0x4803); err != nil || uid != 1001 {
		t.Errorf("IPv4 lookup regressed: uid=%d err=%v, want 1001", uid, err)
	}
}

package proxy

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// TestIdentifyAgentByUID_FailsClosed pins the contract the Jev decision
// endpoint relies on (hivecommons/hive#8939): identity comes from the socket
// UID and nothing else. A self-asserted Proxy-Authorization header is ignored
// even with HIVE_PROXY_ADVISORY_OK=true — the escape hatch identifyAgentFromReq
// honours — and a missing UID map or an unmapped socket yields "".
func TestIdentifyAgentByUID_FailsClosed(t *testing.T) {
	t.Setenv("HIVE_PROXY_ADVISORY_OK", "true")
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:18446/v1/decide", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	req.Header.Set("Proxy-Authorization", "hive quality")

	// No UID map at all → unidentified, whatever the header claims.
	p := &GitHubProxy{proxyAdvisoryOK: true}
	if got := p.IdentifyAgentByUID(req); got != "" {
		t.Fatalf("no UID map: got %q, want \"\"", got)
	}
	if got := p.identifyAgentFromReq(req); got != "quality" {
		t.Fatalf("precondition: the MITM path DOES accept the header under advisory-ok (got %q); the UID-only path must differ", got)
	}

	// UID map loaded, socket table has the port under a MAPPED uid → that agent.
	dir := t.TempDir()
	tcp := filepath.Join(dir, "tcp")
	tcp6 := filepath.Join(dir, "tcp6")
	// /proc/net/tcp layout: header, then "sl local rem st ... uid ..." (uid is field 7).
	if err := os.WriteFile(tcp, []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100007F:A8CA 0100007F:480E 01 00000000:00000000 00:00000000 00000000  2003        0 1 0000000000000000 100 0 0 10 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte("header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTCP, oldTCP6 := procNetTCPPath, procNetTCP6Path
	procNetTCPPath, procNetTCP6Path = tcp, tcp6
	t.Cleanup(func() { procNetTCPPath, procNetTCP6Path = oldTCP, oldTCP6 })

	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"scanner": 2003}}
	if got := p.IdentifyAgentByUID(req); got != "scanner" {
		t.Fatalf("mapped uid: got %q, want scanner (header claimed quality)", got)
	}

	// Mapped socket, but the uid is not an agent → unidentified, header ignored.
	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"quality": 2002}}
	if got := p.IdentifyAgentByUID(req); got != "" {
		t.Fatalf("unmapped uid: got %q, want \"\"", got)
	}
	if got := p.IdentifyAgentByUID(nil); got != "" {
		t.Fatalf("nil request: got %q", got)
	}
}

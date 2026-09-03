package proxy

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// agentUIDFixture is the UID the fake /proc/net/tcp table attributes the
// agent's socket to, and the UID the fake uid map resolves to "scanner".
const agentUIDFixture = 2007

// writeProcNetFixture points the procnet parser at a synthetic socket table in
// which localPort is owned by agentUIDFixture, mirroring the real loopback
// layout: the agent's socket (ephemeral local port -> proxy port) alongside the
// proxy's own accepted socket for the same pair, owned by a different UID.
func writeProcNetFixture(t *testing.T, localPort, proxyPort int) {
	t.Helper()
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// Row 0: the proxy's accepted socket, owned by the proxy UID (1001).
	// Row 1: the agent's socket, owned by the agent UID — the one to find.
	rows := fmt.Sprintf(
		"   0: 0100007F:%04X 0100007F:%04X 01 00000000:00000000 00:00000000 00000000  1001        0 1 1\n"+
			"   1: 0100007F:%04X 0100007F:%04X 01 00000000:00000000 00:00000000 00000000  %d        0 2 1\n",
		proxyPort, localPort, localPort, proxyPort, agentUIDFixture)

	dir := t.TempDir()
	tcp4 := filepath.Join(dir, "tcp")
	tcp6 := filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcp4, []byte(header+rows), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTCP, oldTCP6 := procNetTCPPath, procNetTCP6Path
	procNetTCPPath, procNetTCP6Path = tcp4, tcp6
	t.Cleanup(func() { procNetTCPPath, procNetTCP6Path = oldTCP, oldTCP6 })
}

// TestIdentifyAgentOnReadRequestConnection is the regression test for the
// auto-merge outage: the CONNECT path parses its request with http.ReadRequest,
// which — unlike http.Server — leaves Request.RemoteAddr EMPTY. Identifying the
// agent from the request therefore found no UID, returned "", and every
// write-capable agent was silently downgraded to ADVISORY and blocked.
//
// The test drives a real loopback connection so the peer address is real, then
// asserts the agent is identified from a request whose RemoteAddr is empty
// exactly as http.ReadRequest leaves it. Identifying from r.RemoteAddr fails
// this test; identifying from the connection passes it.
func TestIdentifyAgentOnReadRequestConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// From the proxy's side, the peer is the client's ephemeral port — the port
	// the socket table attributes to the agent UID.
	_, peerPortStr, err := net.SplitHostPort(server.RemoteAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	var peerPort int
	if _, err := fmt.Sscanf(peerPortStr, "%d", &peerPort); err != nil {
		t.Fatal(err)
	}
	_, proxyPortStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var proxyPort int
	if _, err := fmt.Sscanf(proxyPortStr, "%d", &proxyPort); err != nil {
		t.Fatal(err)
	}
	writeProcNetFixture(t, peerPort, proxyPort)

	p := &GitHubProxy{
		uidMap: &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture}},
		logger: slog.Default(),
	}

	// A request shaped exactly as http.ReadRequest produces it: no RemoteAddr,
	// and no Proxy-Authorization header (agents do not send one).
	req, err := http.NewRequest(http.MethodConnect, "https://api.github.com:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.RemoteAddr = ""

	if got := p.identifyAgentFromConn(server, req); got != "scanner" {
		t.Fatalf("agent not identified on the ReadRequest path: got %q, want %q. "+
			"http.ReadRequest leaves RemoteAddr empty, so identification must come "+
			"from the connection — otherwise every agent silently degrades to ADVISORY.", got, "scanner")
	}

	// Guard the trap itself: prove RemoteAddr really is unusable here, so a
	// future refactor back to identifyAgentFromReq cannot look correct.
	if got := p.identifyAgentFromReq(req); got != "" {
		t.Fatalf("expected request-based identification to yield nothing with an empty RemoteAddr, got %q", got)
	}
}

// TestIdentifyAgentFromConnDoesNotTrustHeaderByDefault is identifyAgentFromConn's
// half of the N7 (#3841) regression: on the SAME raw-connection path the
// previous test exercises, a connection whose UID can't be resolved (net.Pipe's
// RemoteAddr is not a host:port, so identifyAgentByUID fails immediately) must
// not fall back to trusting a self-asserted Proxy-Authorization header — unless
// the deployment has explicitly opted into advisory mode.
func TestIdentifyAgentFromConnDoesNotTrustHeaderByDefault(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	req, err := http.NewRequest(http.MethodConnect, "https://api.github.com:443", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Proxy-Authorization", "hive supervisor")

	p := &GitHubProxy{
		uidMap: &agent.UIDMap{Agents: map[string]int{"scanner": agentUIDFixture}},
		logger: slog.Default(),
	}

	if got := p.identifyAgentFromConn(server, req); got != "" {
		t.Errorf("expected the self-asserted header to be ignored (UID lookup failed, not advisory), got %q", got)
	}

	p.proxyAdvisoryOK = true
	if got := p.identifyAgentFromConn(server, req); got != "supervisor" {
		t.Errorf("expected supervisor with proxyAdvisoryOK set, got %q", got)
	}
}

// TestUpdateBranchRequiresMergeMode covers the second half of the outage:
// PUT .../update-branch matched no rule at all, so AllowedByMode fell through
// to its deny-by-default and refused it at EVERY mode — including the trusted
// ISSUES_PRS_MERGE tier that is supposed to land PRs.
func TestUpdateBranchRequiresMergeMode(t *testing.T) {
	const path = "/repos/kubestellar/console/pulls/22110/update-branch"

	if !AllowedByMode(agent.ModeIssuesPRsMerge, "PUT", path) {
		t.Error("ISSUES_PRS_MERGE must be allowed to update a PR branch — it is part of landing a PR")
	}

	// Anything below merge authority must still be refused.
	for _, mode := range []agent.AgentMode{
		agent.ModeAdvisory,
		agent.ModeIssuesOnly,
		agent.ModeIssuesAndPRs,
	} {
		if AllowedByMode(mode, "PUT", path) {
			t.Errorf("mode %v must NOT be allowed to update a PR branch", mode)
		}
	}
}

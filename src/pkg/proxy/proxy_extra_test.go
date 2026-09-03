package proxy

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

func TestTransfer(t *testing.T) {
	s1, c1 := net.Pipe()
	s2, c2 := net.Pipe()

	go func() {
		c1.Write([]byte("hello from src"))
		c1.Close()
	}()

	go transfer(s2, s1)

	buf := make([]byte, 100)
	n, err := c2.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read error: %v", err)
	}
	if string(buf[:n]) != "hello from src" {
		t.Errorf("got %q, want 'hello from src'", buf[:n])
	}
	c2.Close()
}

// TestIdentifyAgentFromReqWithProxyAuth is the N7 (#3841) regression test: a
// self-asserted Proxy-Authorization header must NOT be trusted as identity
// when the UID map is unavailable and the deployment has not explicitly opted
// into advisory mode — otherwise any process on the proxy's loopback port
// could claim to be any agent.
func TestIdentifyAgentFromReqWithProxyAuth(t *testing.T) {
	p := &GitHubProxy{
		uidMap:     nil,
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("scanner:token123")))

	got := p.identifyAgentFromReq(req)
	if got != "" {
		t.Errorf("expected the self-asserted header to be ignored (no UID map, not advisory), got %q", got)
	}
}

// TestIdentifyAgentFromReqWithProxyAuthAdvisoryOK covers the explicit opt-out:
// with proxyAdvisoryOK set (HIVE_PROXY_ADVISORY_OK=true), the same header IS
// trusted — preserving native/dev deployments that have no per-agent UID
// separation at all and would otherwise lose agent identification entirely.
func TestIdentifyAgentFromReqWithProxyAuthAdvisoryOK(t *testing.T) {
	p := &GitHubProxy{
		uidMap:          nil,
		proxyAdvisoryOK: true,
		violations:      make(map[string]int),
		logger:          slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("scanner:token123")))

	got := p.identifyAgentFromReq(req)
	if got != "scanner" {
		t.Errorf("expected 'scanner' with proxyAdvisoryOK set, got %q", got)
	}
}

func TestIdentifyAgentFromReqNoAuth(t *testing.T) {
	p := &GitHubProxy{
		uidMap:     nil,
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
	got := p.identifyAgentFromReq(req)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// TestIdentifyAgentFromReqUIDMapNoIptables: a UID map that loaded but whose
// lookup fails for this connection (here, IptablesActive is false so the
// redirect that would make UID attribution meaningful never installed) is the
// SAME "unforgeable identification did not succeed" case as a nil map (N7,
// #3841) — the header must not be trusted without proxyAdvisoryOK either.
func TestIdentifyAgentFromReqUIDMapNoIptables(t *testing.T) {
	p := &GitHubProxy{
		uidMap:     &agent.UIDMap{IptablesActive: false},
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("quality:token")))

	got := p.identifyAgentFromReq(req)
	if got != "" {
		t.Errorf("without a UID match, the self-asserted header must be ignored (not advisory), got %q", got)
	}
}

func TestReadAgentModeFromTempFile(t *testing.T) {
	tmpDir := t.TempDir()
	agentName := "test-read-mode-agent"
	modePath := tmpDir + "/" + agentName

	os.WriteFile(modePath, []byte("ISSUES_AND_PRS\n"), 0644)

	origPrefix := modeFilePrefix
	_ = origPrefix
	// Can't override const, but test the default path behavior
	got := readAgentMode(agentName)
	// Expected: mode file not at /tmp/.hive-mode-<name>, so defaults to advisory
	if got != agent.ModeAdvisory {
		t.Errorf("readAgentMode(%q) = %v, want %v (default advisory)", agentName, got, agent.ModeAdvisory)
	}
}

func TestReadAgentModeAllModes(t *testing.T) {
	// Test that empty name returns ADVISORY
	if readAgentMode("") != agent.ModeAdvisory {
		t.Error("empty name should return ADVISORY")
	}
	// Test nonexistent returns ADVISORY
	if readAgentMode("nonexistent-agent-zzz-999") != agent.ModeAdvisory {
		t.Error("nonexistent should return ADVISORY")
	}
}

func TestExtractSNIShortHandshake(t *testing.T) {
	// Valid TLS record header but handshake too short
	data := []byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00}
	sni := extractSNI(data)
	if sni != "" {
		t.Errorf("short handshake should return empty, got %q", sni)
	}
}

func TestExtractSNITruncatedRecord(t *testing.T) {
	// Record length says more bytes than available
	data := []byte{0x16, 0x03, 0x01, 0x00, 0xFF}
	sni := extractSNI(data)
	if sni != "" {
		t.Errorf("truncated record should return empty, got %q", sni)
	}
}

func TestExtractSNIMinimalValid(t *testing.T) {
	// Less than 5 bytes
	if extractSNI([]byte{0x16, 0x03}) != "" {
		t.Error("< 5 bytes should return empty")
	}
	if extractSNI(nil) != "" {
		t.Error("nil should return empty")
	}
	if extractSNI([]byte{}) != "" {
		t.Error("empty should return empty")
	}
}

func TestPrefixConnReadFull(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	go func() {
		client.Write([]byte("world"))
		client.Close()
	}()

	pc := &prefixConn{Conn: server, prefix: []byte("hello ")}

	var result strings.Builder
	buf := make([]byte, 4)
	for {
		n, err := pc.Read(buf)
		if n > 0 {
			result.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if result.String() != "hello world" {
		t.Errorf("got %q, want 'hello world'", result.String())
	}
}

func TestRecordViolationMultipleAgents(t *testing.T) {
	p := &GitHubProxy{
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	p.recordViolation("quality", "PUT", "/repos/org/repo/pulls/1/merge")

	if p.AgentViolations("scanner") != 2 {
		t.Errorf("scanner = %d, want 2", p.AgentViolations("scanner"))
	}
	if p.AgentViolations("quality") != 1 {
		t.Errorf("quality = %d, want 1", p.AgentViolations("quality"))
	}
}

func TestViolationsSnapshotIsolated(t *testing.T) {
	p := &GitHubProxy{
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")

	snap := p.Violations()
	snap["scanner"] = 999

	if p.AgentViolations("scanner") == 999 {
		t.Error("modifying snapshot should not affect original")
	}
}

func TestForgeCertExpiry(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}

	p := &GitHubProxy{
		caCert:    caCert,
		caX509:    caX509,
		logger:    slog.Default(),
		certCache: make(map[string]cachedCert),
	}

	cert, err := p.forgeCert("test.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Manually expire the cache
	p.certMu.Lock()
	p.certCache["test.example.com"] = cachedCert{
		cert:      cert,
		expiresAt: time.Now().Add(-time.Hour),
	}
	p.certMu.Unlock()

	// Should generate a new cert
	cert2, err := p.forgeCert("test.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cert2.PrivateKey == nil {
		t.Error("regenerated cert should have private key")
	}
}

func TestAllowedByModeComprehensive(t *testing.T) {
	tests := []struct {
		mode   agent.AgentMode
		method string
		path   string
		want   bool
	}{
		// Advisory can read anything
		{agent.ModeAdvisory, "GET", "/repos/org/repo/issues", true},
		{agent.ModeAdvisory, "HEAD", "/repos/org/repo", true},
		{agent.ModeAdvisory, "OPTIONS", "/", true},

		// Advisory cannot write
		{agent.ModeAdvisory, "POST", "/repos/org/repo/issues", false},
		{agent.ModeAdvisory, "POST", "/repos/org/repo/pulls", false},
		{agent.ModeAdvisory, "PUT", "/repos/org/repo/pulls/1/merge", false},
		{agent.ModeAdvisory, "DELETE", "/repos/org/repo/git/refs/heads/branch", false},

		// IssuesOnly can create issues but not PRs
		{agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues", true},
		{agent.ModeIssuesOnly, "PATCH", "/repos/org/repo/issues/1", true},
		{agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/1/comments", true},
		{agent.ModeIssuesOnly, "POST", "/repos/org/repo/pulls", false},

		// IssuesAndPRs can push + patch PRs but NOT create them directly
		// (POST /pulls is a hard deny — agents use hive-open-pr).
		{agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls", false},
		{agent.ModeIssuesAndPRs, "PATCH", "/repos/org/repo/pulls/42", true},
		{agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/issues", true},
		{agent.ModeIssuesAndPRs, "PUT", "/repos/org/repo/pulls/1/merge", false},

		// IssuesPRsMerge can create issues, but cannot merge directly (PUT merge
		// is a hard deny for every mode — H1, use the hive-merge relay) nor POST
		// /pulls directly (direct PR creation is denied for every mode).
		{agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/1/merge", false},
		{agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/pulls", false},
		{agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/issues", true},
	}

	for _, tt := range tests {
		got := AllowedByMode(tt.mode, tt.method, tt.path)
		if got != tt.want {
			t.Errorf("AllowedByMode(%v, %q, %q) = %v, want %v",
				tt.mode, tt.method, tt.path, got, tt.want)
		}
	}
}

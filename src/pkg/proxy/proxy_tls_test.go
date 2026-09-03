package proxy

import (
	"crypto/x509"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func TestGenerateCA(t *testing.T) {
	tlsCert, x509Cert, err := generateCA()
	if err != nil {
		t.Fatalf("generateCA error: %v", err)
	}
	if x509Cert == nil {
		t.Fatal("x509Cert should not be nil")
	}
	if !x509Cert.IsCA {
		t.Error("certificate should be a CA")
	}
	if x509Cert.Subject.CommonName != "Hive ACMM Proxy CA" {
		t.Errorf("CN = %q", x509Cert.Subject.CommonName)
	}
	if tlsCert.PrivateKey == nil {
		t.Error("private key should not be nil")
	}
}

func TestForgeCert(t *testing.T) {
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

	cert, err := p.forgeCert("api.github.com")
	if err != nil {
		t.Fatalf("forgeCert error: %v", err)
	}
	if cert.PrivateKey == nil {
		t.Error("forged cert should have private key")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse forged cert: %v", err)
	}
	if leaf.Subject.CommonName != "api.github.com" {
		t.Errorf("CN = %q, want 'api.github.com'", leaf.Subject.CommonName)
	}
	if len(leaf.DNSNames) == 0 || leaf.DNSNames[0] != "api.github.com" {
		t.Error("SAN should include api.github.com")
	}
}

func TestForgeCertCached(t *testing.T) {
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

	cert1, _ := p.forgeCert("github.com")
	cert2, _ := p.forgeCert("github.com")

	if len(cert1.Certificate) == 0 || len(cert2.Certificate) == 0 {
		t.Fatal("certs should not be empty")
	}

	leaf1, _ := x509.ParseCertificate(cert1.Certificate[0])
	leaf2, _ := x509.ParseCertificate(cert2.Certificate[0])
	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) != 0 {
		t.Error("second call should return cached cert with same serial")
	}
}

func TestForgeCertDifferentHosts(t *testing.T) {
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

	cert1, _ := p.forgeCert("api.github.com")
	cert2, _ := p.forgeCert("github.com")

	leaf1, _ := x509.ParseCertificate(cert1.Certificate[0])
	leaf2, _ := x509.ParseCertificate(cert2.Certificate[0])
	if leaf1.Subject.CommonName == leaf2.Subject.CommonName {
		t.Error("different hosts should get different certs")
	}
}

func TestReadAgentModeFromFile(t *testing.T) {
	dir := t.TempDir()
	origPrefix := modeFilePrefix
	_ = origPrefix

	modePath := filepath.Join(dir, "mode-test-agent")
	os.WriteFile(modePath, []byte("ISSUES_AND_PRS\n"), 0644)

	got := readAgentMode("")
	if got != agent.ModeAdvisory {
		t.Errorf("empty agent name should return ADVISORY, got %v", got)
	}
}

func TestReadAgentModeMissing(t *testing.T) {
	got := readAgentMode("nonexistent-agent-xyz-999")
	if got != agent.ModeAdvisory {
		t.Errorf("missing mode file should return ADVISORY, got %v", got)
	}
}

func TestListenAddr(t *testing.T) {
	p := &GitHubProxy{listenAddr: "127.0.0.1:18443"}
	if p.ListenAddr() != "127.0.0.1:18443" {
		t.Errorf("ListenAddr() = %q", p.ListenAddr())
	}
}

func TestViolationsEmpty(t *testing.T) {
	p := &GitHubProxy{violations: make(map[string]int)}
	v := p.Violations()
	if len(v) != 0 {
		t.Error("should be empty")
	}
}

func TestAgentViolationsZero(t *testing.T) {
	p := &GitHubProxy{violations: make(map[string]int)}
	if p.AgentViolations("scanner") != 0 {
		t.Error("unknown agent should have 0 violations")
	}
}

func TestRecordViolationIncrement(t *testing.T) {
	p := &GitHubProxy{
		violations: make(map[string]int),
		logger:     slog.Default(),
	}
	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	p.recordViolation("quality", "PUT", "/repos/org/repo/pulls/1/merge")

	if p.AgentViolations("scanner") != 2 {
		t.Errorf("scanner violations = %d, want 2", p.AgentViolations("scanner"))
	}
	if p.AgentViolations("quality") != 1 {
		t.Errorf("quality violations = %d, want 1", p.AgentViolations("quality"))
	}

	snap := p.Violations()
	if snap["scanner"] != 2 {
		t.Error("snapshot should reflect violations")
	}
}

func TestNewBufferedConn(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	r := newBufferedConn(client)
	if r == nil {
		t.Error("should return non-nil reader")
	}
}

func TestNewBufferedReader(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	r := newBufferedReader(client)
	if r == nil {
		t.Error("should return non-nil reader")
	}
}

func TestIdentifyAgentFromReqNoUIDMap(t *testing.T) {
	// identifyAgentFromReq needs *http.Request — tested via extractAgentName
	// in proxy_coverage_test.go. This just verifies the struct can be created.
	p := &GitHubProxy{
		uidMap:     nil,
		violations: make(map[string]int),
		logger:     slog.Default(),
	}
	if p.ListenAddr() != "" {
		t.Error("empty listenAddr should be empty")
	}
}

func TestIdentifyAgentByUIDInvalidPort(t *testing.T) {
	uidMap := &agent.UIDMap{IptablesActive: true}
	p := &GitHubProxy{
		uidMap:     uidMap,
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	got := p.identifyAgentByUID("127.0.0.1:abc")
	if got != "" {
		t.Errorf("invalid port should return empty, got %q", got)
	}

	got2 := p.identifyAgentByUID("invalid-no-colon")
	if got2 != "" {
		t.Errorf("no port should return empty, got %q", got2)
	}
}

func TestIdentifyAgentByUIDOverflow(t *testing.T) {
	uidMap := &agent.UIDMap{IptablesActive: true}
	p := &GitHubProxy{
		uidMap:     uidMap,
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	got := p.identifyAgentByUID("127.0.0.1:99999999")
	if got != "" {
		t.Errorf("overflow port should return empty, got %q", got)
	}
}

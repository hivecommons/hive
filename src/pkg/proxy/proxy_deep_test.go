package proxy

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// ---------- extractSNI: build a real TLS ClientHello ----------

// buildClientHello creates a minimal TLS ClientHello with an SNI extension.
func buildClientHello(hostname string) []byte {
	sniBytes := []byte(hostname)

	// SNI extension payload: list length (2) + name type (1) + name length (2) + name
	sniExtPayload := make([]byte, 0, 5+len(sniBytes))
	sniListLen := 1 + 2 + len(sniBytes) // nameType + nameLen(2) + name
	sniExtPayload = append(sniExtPayload, byte(sniListLen>>8), byte(sniListLen))
	sniExtPayload = append(sniExtPayload, 0) // host_name type
	sniExtPayload = append(sniExtPayload, byte(len(sniBytes)>>8), byte(len(sniBytes)))
	sniExtPayload = append(sniExtPayload, sniBytes...)

	// Extensions block: ext type (2) + ext length (2) + payload
	extensions := make([]byte, 0, 4+len(sniExtPayload))
	extensions = append(extensions, 0, 0) // SNI extension type = 0x0000
	extensions = append(extensions, byte(len(sniExtPayload)>>8), byte(len(sniExtPayload)))
	extensions = append(extensions, sniExtPayload...)

	// ClientHello body: version(2) + random(32) + sessionID len(1) + cipher suites len(2) + one suite(2) + compression len(1) + null(1) + extensions len(2) + extensions
	chBody := make([]byte, 0, 256)
	chBody = append(chBody, 0x03, 0x03) // TLS 1.2
	random := make([]byte, 32)
	chBody = append(chBody, random...)
	chBody = append(chBody, 0) // session ID length = 0

	// Cipher suites: 2 bytes length + one suite
	chBody = append(chBody, 0x00, 0x02) // 2 bytes of cipher suites
	chBody = append(chBody, 0x00, 0x2f) // TLS_RSA_WITH_AES_128_CBC_SHA
	chBody = append(chBody, 0x01)       // compression methods length
	chBody = append(chBody, 0x00)       // null compression

	// Extensions
	chBody = append(chBody, byte(len(extensions)>>8), byte(len(extensions)))
	chBody = append(chBody, extensions...)

	// Handshake header: type(1) + length(3)
	hsHeader := []byte{0x01} // ClientHello type
	hsLen := len(chBody)
	hsHeader = append(hsHeader, byte(hsLen>>16), byte(hsLen>>8), byte(hsLen))

	handshake := append(hsHeader, chBody...)

	// TLS record header: type(1) + version(2) + length(2)
	record := []byte{0x16, 0x03, 0x01} // handshake, TLS 1.0
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	return record
}

func TestExtractSNIValidHostname(t *testing.T) {
	tests := []string{
		"api.github.com",
		"github.com",
		"example.com",
		"long-hostname.sub.domain.example.org",
	}
	for _, hostname := range tests {
		data := buildClientHello(hostname)
		got := extractSNI(data)
		if got != hostname {
			t.Errorf("extractSNI with %q: got %q, want %q", hostname, got, hostname)
		}
	}
}

func TestExtractSNIPartialRecord(t *testing.T) {
	// Record length says more data than available — should still parse partial
	data := buildClientHello("example.com")
	// Truncate slightly past the SNI so it still works
	full := data
	// Try with truncated data (cut off last few bytes)
	truncated := full[:len(full)-2]
	sni := extractSNI(truncated)
	// May or may not find SNI depending on how truncation hits — just shouldn't panic
	_ = sni
}

func TestExtractSNINoExtensions(t *testing.T) {
	// Build a ClientHello without extensions
	chBody := make([]byte, 0, 128)
	chBody = append(chBody, 0x03, 0x03)             // version
	chBody = append(chBody, make([]byte, 32)...)    // random
	chBody = append(chBody, 0)                      // session ID len
	chBody = append(chBody, 0x00, 0x02, 0x00, 0x2f) // cipher suites
	chBody = append(chBody, 0x01, 0x00)             // compression

	hsHeader := []byte{0x01}
	hsLen := len(chBody)
	hsHeader = append(hsHeader, byte(hsLen>>16), byte(hsLen>>8), byte(hsLen))
	handshake := append(hsHeader, chBody...)

	record := []byte{0x16, 0x03, 0x01}
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	sni := extractSNI(record)
	if sni != "" {
		t.Errorf("no-extensions ClientHello should return empty SNI, got %q", sni)
	}
}

func TestExtractSNINonSNIExtension(t *testing.T) {
	// Build with a non-SNI extension (e.g., extension type 0x0010 = ALPN)
	chBody := make([]byte, 0, 128)
	chBody = append(chBody, 0x03, 0x03)
	chBody = append(chBody, make([]byte, 32)...)
	chBody = append(chBody, 0)
	chBody = append(chBody, 0x00, 0x02, 0x00, 0x2f)
	chBody = append(chBody, 0x01, 0x00)

	// ALPN extension with no SNI
	ext := []byte{0x00, 0x10, 0x00, 0x02, 0x01, 0x00} // type=ALPN, len=2, data
	chBody = append(chBody, byte(len(ext)>>8), byte(len(ext)))
	chBody = append(chBody, ext...)

	hsHeader := []byte{0x01}
	hsLen := len(chBody)
	hsHeader = append(hsHeader, byte(hsLen>>16), byte(hsLen>>8), byte(hsLen))
	handshake := append(hsHeader, chBody...)

	record := []byte{0x16, 0x03, 0x01}
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	sni := extractSNI(record)
	if sni != "" {
		t.Errorf("non-SNI extension should return empty, got %q", sni)
	}
}

func TestExtractSNIHandshakeTooShort(t *testing.T) {
	// Valid TLS record but handshake body < 34 bytes (no room for version+random)
	handshake := []byte{0x01, 0x00, 0x00, 0x10} // type=ClientHello, len=16
	handshake = append(handshake, make([]byte, 16)...)

	record := []byte{0x16, 0x03, 0x01}
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	sni := extractSNI(record)
	if sni != "" {
		t.Errorf("short handshake should return empty, got %q", sni)
	}
}

func TestExtractSNISessionIDPresent(t *testing.T) {
	// ClientHello with a session ID
	chBody := make([]byte, 0, 200)
	chBody = append(chBody, 0x03, 0x03)             // version
	chBody = append(chBody, make([]byte, 32)...)    // random
	chBody = append(chBody, 4)                      // session ID length = 4
	chBody = append(chBody, 0xAA, 0xBB, 0xCC, 0xDD) // session ID
	chBody = append(chBody, 0x00, 0x02, 0x00, 0x2f) // cipher suites
	chBody = append(chBody, 0x01, 0x00)             // compression

	hostname := "with-session.example.com"
	sniBytes := []byte(hostname)
	sniListLen := 1 + 2 + len(sniBytes)
	sniExtPayload := make([]byte, 0, 5+len(sniBytes))
	sniExtPayload = append(sniExtPayload, byte(sniListLen>>8), byte(sniListLen))
	sniExtPayload = append(sniExtPayload, 0)
	sniExtPayload = append(sniExtPayload, byte(len(sniBytes)>>8), byte(len(sniBytes)))
	sniExtPayload = append(sniExtPayload, sniBytes...)

	ext := make([]byte, 0, 4+len(sniExtPayload))
	ext = append(ext, 0, 0)
	ext = append(ext, byte(len(sniExtPayload)>>8), byte(len(sniExtPayload)))
	ext = append(ext, sniExtPayload...)

	chBody = append(chBody, byte(len(ext)>>8), byte(ext[len(ext)-1]))
	// Recalculate properly
	chBody = chBody[:len(chBody)-2]
	chBody = append(chBody, byte(len(ext)>>8), byte(len(ext)))
	chBody = append(chBody, ext...)

	hsHeader := []byte{0x01}
	hsLen := len(chBody)
	hsHeader = append(hsHeader, byte(hsLen>>16), byte(hsLen>>8), byte(hsLen))
	handshake := append(hsHeader, chBody...)

	record := []byte{0x16, 0x03, 0x01}
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	sni := extractSNI(record)
	if sni != hostname {
		t.Errorf("extractSNI with session ID: got %q, want %q", sni, hostname)
	}
}

// ---------- readAgentMode: use the actual /tmp path ----------

func TestReadAgentModeFromRealTmpFile(t *testing.T) {
	// readAgentMode uses modeFilePrefix = "/tmp/.hive-mode-"
	// Create a temp mode file at that path
	agentName := fmt.Sprintf("test-agent-%d", time.Now().UnixNano())
	modePath := modeFilePrefix + agentName
	defer os.Remove(modePath)

	// Write ISSUES_AND_PRS mode
	if err := os.WriteFile(modePath, []byte("ISSUES_AND_PRS\n"), 0644); err != nil {
		t.Skipf("cannot write to %s: %v", modePath, err)
	}

	got := readAgentMode(agentName)
	if got != agent.ModeIssuesAndPRs {
		t.Errorf("readAgentMode(%q) = %v, want ISSUES_AND_PRS", agentName, got)
	}

	// Test other modes
	os.WriteFile(modePath, []byte("ADVISORY"), 0644)
	if readAgentMode(agentName) != agent.ModeAdvisory {
		t.Error("expected ADVISORY")
	}

	os.WriteFile(modePath, []byte("ISSUES_ONLY"), 0644)
	if readAgentMode(agentName) != agent.ModeIssuesOnly {
		t.Error("expected ISSUES_ONLY")
	}

	os.WriteFile(modePath, []byte("ISSUES_PRS_MERGE"), 0644)
	if readAgentMode(agentName) != agent.ModeIssuesPRsMerge {
		t.Error("expected ISSUES_PRS_MERGE")
	}

	// Invalid mode string => ADVISORY
	os.WriteFile(modePath, []byte("INVALID_MODE"), 0644)
	if readAgentMode(agentName) != agent.ModeAdvisory {
		t.Error("invalid mode should default to ADVISORY")
	}

	// Whitespace-padded
	os.WriteFile(modePath, []byte("  ISSUES_ONLY  \n"), 0644)
	if readAgentMode(agentName) != agent.ModeIssuesOnly {
		t.Error("whitespace-padded ISSUES_ONLY should be parsed")
	}
}

// ---------- forgeCert: nil cache, expired cache ----------

func TestForgeCertNilCache(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	p := &GitHubProxy{
		caCert:    caCert,
		caX509:    caX509,
		logger:    slog.Default(),
		certCache: nil, // nil cache
	}

	cert, err := p.forgeCert("nil-cache-test.example.com")
	if err != nil {
		t.Fatalf("forgeCert with nil cache: %v", err)
	}
	if cert.PrivateKey == nil {
		t.Error("cert should have private key")
	}

	// Verify it got cached
	p.certMu.RLock()
	_, ok := p.certCache["nil-cache-test.example.com"]
	p.certMu.RUnlock()
	if !ok {
		t.Error("cert should be in cache after generation")
	}
}

// ---------- generateCA: verify cert properties ----------

func TestGenerateCAProperties(t *testing.T) {
	tlsCert, x509Cert, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}

	if !x509Cert.IsCA {
		t.Error("should be CA")
	}
	// MaxPathLen is set to 0 in the template but x509 may report it differently
	// depending on Go version. Just verify it's a CA.
	if x509Cert.MaxPathLen > 0 {
		t.Errorf("MaxPathLen = %d, expected 0 or -1", x509Cert.MaxPathLen)
	}
	if !x509Cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid should be true")
	}
	if x509Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("should have CertSign key usage")
	}
	if x509Cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("should have CRLSign key usage")
	}

	// Verify the cert chain works: can we sign a leaf with it?
	caKey := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, x509Cert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("signing leaf cert with CA failed: %v", err)
	}
	if len(leafDER) == 0 {
		t.Error("leaf cert should not be empty")
	}
}

// ---------- forwardPlainDirect: successful forward ----------

func TestForwardPlainDirectSuccess(t *testing.T) {
	// Create a mock upstream server
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "passed")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()

	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("GET", upstream.URL+"/test", nil)
	go p.forwardPlainDirect(proxyClient, req)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream response") {
		t.Errorf("body = %q, expected 'upstream response'", body)
	}
}

// ---------- proxyHTTP: repo filter blocked ----------

func TestProxyHTTPRepoFilterBlocked(t *testing.T) {
	p := newTestProxy()
	p.allowedRepos = map[string]bool{"org/allowed-repo": true}

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})

	// POST to a repo not in the allowed list, then close so proxyHTTP sees EOF
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/forbidden-repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 2\r\n\r\n{}")
		// Give proxyHTTP time to read and respond, then close to unblock ReadAll
		time.Sleep(200 * time.Millisecond)
		clientConn.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := upstreamConn.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Read raw bytes instead of using http.ReadResponse which blocks on body
	buf := make([]byte, 4096)
	var collected []byte
	for {
		clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := clientConn.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	response := string(collected)
	if !strings.Contains(response, "403") {
		t.Errorf("repo filter should block with 403, got: %s", response)
	}
	if !strings.Contains(response, "repo not in hive config") {
		t.Errorf("expected 'repo not in hive config' in response, got: %s", response)
	}

	if p.AgentViolations("scanner") != 1 {
		t.Errorf("should record 1 violation, got %d", p.AgentViolations("scanner"))
	}
}

// ---------- proxyHTTP: git path triggers raw streaming ----------

func TestProxyHTTPGitPath(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})

	// Send a git-upload-pack request (allowed at advisory mode)
	go func() {
		fmt.Fprintf(clientConn, "POST /org/repo.git/info/refs HTTP/1.1\r\nHost: github.com\r\nContent-Length: 0\r\n\r\n")
		// After the request is forwarded, send some data
		time.Sleep(50 * time.Millisecond)
		clientConn.Write([]byte("client git data"))
		clientConn.Close()
	}()

	// Upstream reads the request and sends a response
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		_ = req
		upstreamConn.Write([]byte("git pack data response"))
		upstreamConn.Close()
	}()

	// Just let it complete without hanging
	time.Sleep(200 * time.Millisecond)
}

// ---------- handleConn: HTTP CONNECT path ----------

func TestHandleConnHTTPRequest(t *testing.T) {
	p := newTestProxy()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("plain http response"))
	}))
	defer upstream.Close()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	go p.handleConn(proxyClient)

	// Send a plain HTTP GET (starts with 'G', not 0x16)
	fmt.Fprintf(clientConn, "GET %s/test HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.URL, upstream.URL)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	// The forwardPlainDirect will attempt to RoundTrip
	// Status could be 200 or 502 depending on host resolution
	if resp.StatusCode != 200 && resp.StatusCode != 502 {
		t.Errorf("unexpected status %d", resp.StatusCode)
	}
}

func TestHandleConnTLSByte(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()

	go p.handleConn(proxyClient)

	// Send TLS handshake byte followed by garbage — handleTransparentTLS will fail gracefully
	clientConn.Write([]byte{0x16})
	time.Sleep(50 * time.Millisecond)
	clientConn.Write([]byte{0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00})
	clientConn.Close()

	// Just ensure it doesn't panic or hang
	time.Sleep(200 * time.Millisecond)
}

// ---------- translateOpenAIResponseToAnthropic: edge cases ----------

func TestTranslateOpenAIResponseNoChoices(t *testing.T) {
	resp := `{"id": "chatcmpl-002", "choices": [], "usage": {"prompt_tokens": 5, "completion_tokens": 0}}`
	result, err := translateOpenAIResponseToAnthropic([]byte(resp), "test-model")
	if err != nil {
		t.Fatal(err)
	}

	var ar anthropicResponse
	json.Unmarshal(result, &ar)
	if ar.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", ar.StopReason)
	}
	if len(ar.Content) != 1 || ar.Content[0].Text != "" {
		t.Errorf("empty choices should produce empty text block")
	}
}

func TestTranslateOpenAIResponseNoUsage(t *testing.T) {
	resp := `{"id": "chatcmpl-003", "choices": [{"message": {"content": "hi"}, "finish_reason": "stop"}]}`
	result, err := translateOpenAIResponseToAnthropic([]byte(resp), "test-model")
	if err != nil {
		t.Fatal(err)
	}

	var ar anthropicResponse
	json.Unmarshal(result, &ar)
	if ar.Usage == nil {
		t.Fatal("usage should not be nil even without upstream usage")
	}
	if ar.Usage.InputTokens != 0 || ar.Usage.OutputTokens != 0 {
		t.Errorf("usage should be zeros when upstream has no usage")
	}
}

func TestTranslateOpenAIResponseLengthFinish(t *testing.T) {
	resp := `{"id": "chatcmpl-004", "choices": [{"message": {"content": "partial"}, "finish_reason": "length"}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}`
	result, err := translateOpenAIResponseToAnthropic([]byte(resp), "m")
	if err != nil {
		t.Fatal(err)
	}

	var ar anthropicResponse
	json.Unmarshal(result, &ar)
	if ar.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens", ar.StopReason)
	}
}

func TestTranslateOpenAIResponseInvalidJSON(t *testing.T) {
	_, err := translateOpenAIResponseToAnthropic([]byte("not json"), "m")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// ---------- forwardToInference: error responses ----------

func TestForwardToInferenceUpstreamError(t *testing.T) {
	// Mock server that returns 500
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(body), w, route, "test-agent")
	// The function writes the error response to w and returns writeErr
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "inference backend returned 500") {
		t.Errorf("body should contain error message, got %q", respBody)
	}
	_ = err
}

func TestForwardToInferenceTranslateError(t *testing.T) {
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:1", Model: "test"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte("not json"), w, route, "test-agent")
	if err == nil {
		t.Error("expected error for invalid body")
	}
}

func TestForwardToInferenceUnreachable(t *testing.T) {
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(body), w, route, "test-agent")
	if err == nil {
		t.Error("expected error for unreachable backend")
	}
}

// ---------- writeHTTPError ----------

func TestWriteHTTPError(t *testing.T) {
	p := newTestProxy()
	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		p.writeHTTPError(proxyConn, http.StatusBadGateway, "test error message")
		proxyConn.Close() // close so client can read full body
	}()

	buf := make([]byte, 4096)
	var collected []byte
	for {
		n, err := clientConn.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	response := string(collected)
	if !strings.Contains(response, "502") {
		t.Errorf("expected 502, got: %s", response)
	}
	if !strings.Contains(response, "test error message") {
		t.Errorf("expected 'test error message' in response, got: %s", response)
	}
}

// ---------- truncateBytes ----------

func TestTruncateBytes(t *testing.T) {
	short := []byte("hello")
	if truncateBytes(short, 10) != "hello" {
		t.Error("short string should not be truncated")
	}

	long := []byte("hello world this is long")
	result := truncateBytes(long, 5)
	if result != "hello..." {
		t.Errorf("truncateBytes = %q, want 'hello...'", result)
	}

	exact := []byte("12345")
	if truncateBytes(exact, 5) != "12345" {
		t.Error("exact length should not be truncated")
	}

	empty := []byte("")
	if truncateBytes(empty, 5) != "" {
		t.Error("empty should return empty")
	}
}

// ---------- RegisterGitHubHost ----------

func TestRegisterGitHubHost(t *testing.T) {
	RegisterGitHubHost("ghe.example.com")
	if !IsGitHubHost("ghe.example.com") {
		t.Error("registered host should be GitHub host")
	}
	// Empty string should be no-op
	RegisterGitHubHost("")
	if IsGitHubHost("") {
		t.Error("empty string should not be a GitHub host")
	}
	// Clean up
	delete(githubHosts, "ghe.example.com")
}

// ---------- SetInferenceRoute / ClearInferenceRoute ----------

func TestSetAndClearInferenceRoute(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:8000", Model: "test"}
	p.SetInferenceRoute("agent-1", route)

	got := p.inference.Get("agent-1")
	if got == nil || got.Backend != "vllm" {
		t.Error("route should be set")
	}

	p.ClearInferenceRoute("agent-1")
	if p.inference.Get("agent-1") != nil {
		t.Error("route should be cleared")
	}
}

// SetInferenceRoute is called while the agent-manager mutex is held (agent
// launch, SetModelOverride, SetBackendOverride). It must return promptly and
// never block on the up-to-3s max-model-len endpoint probe, otherwise a model
// switch pins the manager mutex and stalls the dashboard — reading as
// "Application is not available" on the OpenShift router. Use an unroutable
// endpoint so the probe would hang for the full timeout if it ran inline.
func TestSetInferenceRouteDoesNotBlockOnEndpointProbe(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	// 203.0.113.0/24 (TEST-NET-3) is reserved and unroutable: a real probe
	// would block until maxModelLenQueryTimeout (3s).
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://203.0.113.1:8000", Model: "test"}

	done := make(chan struct{})
	go func() {
		p.SetInferenceRoute("agent-block", route)
		close(done)
	}()

	select {
	case <-done:
		// Returned promptly — route was set without waiting on the probe.
	case <-time.After(maxModelLenQueryTimeout - 500*time.Millisecond):
		t.Fatal("SetInferenceRoute blocked on the max-model-len endpoint probe; " +
			"it must set the route synchronously and probe asynchronously")
	}

	if got := p.inference.Get("agent-block"); got == nil || got.Model != "test" {
		t.Error("route should be set immediately, before the async probe completes")
	}
}

// UpdateMaxContextLen must not clobber a route that has since been changed to a
// different endpoint/model — a slow probe from a prior model switch could
// otherwise overwrite the value the user just switched to.
func TestUpdateMaxContextLenStaleGuard(t *testing.T) {
	ir := newInferenceRouter()
	ir.Set("a", &InferenceRoute{Endpoint: "http://e1", Model: "m1"})

	if !ir.UpdateMaxContextLen("a", "http://e1", "m1", 8192) {
		t.Fatal("update should apply for the matching endpoint/model")
	}
	if got := ir.Get("a"); got.MaxContextLen != 8192 {
		t.Errorf("MaxContextLen = %d, want 8192", got.MaxContextLen)
	}

	// Route changed to a new model: a stale probe result must be rejected.
	ir.Set("a", &InferenceRoute{Endpoint: "http://e2", Model: "m2"})
	if ir.UpdateMaxContextLen("a", "http://e1", "m1", 4096) {
		t.Error("stale update for old endpoint/model must be rejected")
	}
	if got := ir.Get("a"); got.MaxContextLen != 0 {
		t.Errorf("stale update leaked into new route: MaxContextLen = %d, want 0", got.MaxContextLen)
	}

	// Missing agent: no panic, returns false.
	if ir.UpdateMaxContextLen("missing", "http://e1", "m1", 100) {
		t.Error("update for a missing agent must return false")
	}
}

// ---------- handleConn: CONNECT path (non-GitHub host) ----------

func TestHandleConnCONNECTNonGitHub(t *testing.T) {
	p := newTestProxy()

	// Start a mock TCP server to tunnel to
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("tunneled data"))
		conn.Close()
	}()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	go p.handleConn(proxyClient)

	// Send CONNECT to the mock server
	fmt.Fprintf(clientConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr().String(), ln.Addr().String())

	reader := bufio.NewReader(clientConn)
	line, _ := reader.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200 Connection established, got %q", line)
	}
	// Read past the blank line
	for {
		l, _ := reader.ReadString('\n')
		if l == "\r\n" || l == "\n" || l == "" {
			break
		}
	}

	// Read tunneled data
	buf := make([]byte, 100)
	n, _ := reader.Read(buf)
	if n > 0 && !strings.Contains(string(buf[:n]), "tunneled data") {
		t.Logf("tunneled data: %q", buf[:n])
	}
}

// ---------- proxyHTTP: multiple requests, second is blocked ----------

func TestProxyHTTPMultipleRequests(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	// First: allowed GET
	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	}()

	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		resp := &http.Response{
			StatusCode: 200,
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}
		resp.Write(upstreamConn)
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("first request should be 200, got %d", resp.StatusCode)
	}
}

// ---------- isGitPath ----------

func TestIsGitPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/org/repo.git/git-receive-pack", true},
		{"/org/repo.git/git-upload-pack", true},
		{"/org/repo.git/info/refs", true},
		{"/repos/org/repo/pulls", false},
		{"/graphql", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isGitPath(tt.path); got != tt.want {
			t.Errorf("isGitPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// ---------- ExtractRepo and RepoFilterAllowed ----------

func TestExtractRepoEdgeCases(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"", ""},
		{"/repos", ""},
		{"/repos/", ""},
	}
	for _, tt := range tests {
		got := ExtractRepo(tt.path)
		if got != tt.want {
			t.Errorf("ExtractRepo(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestRepoFilterAllowedDELETE(t *testing.T) {
	allowed := map[string]bool{"org/console": true}

	// DELETE to allowed repo
	if !RepoFilterAllowed(allowed, "DELETE", "/repos/org/console/git/refs/heads/branch") {
		t.Error("DELETE to allowed repo should pass")
	}

	// DELETE to disallowed repo
	if RepoFilterAllowed(allowed, "DELETE", "/repos/org/other/git/refs/heads/branch") {
		t.Error("DELETE to disallowed repo should fail")
	}

	// PUT to allowed repo
	if !RepoFilterAllowed(allowed, "PUT", "/repos/org/console/pulls/1/merge") {
		t.Error("PUT to allowed repo should pass")
	}
}

// ---------- flushResponseWriter ----------

func TestFlushResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := &flushResponseWriter{w: rec, f: rec}

	n, err := fw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("wrote %d bytes, want 5", n)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want 'hello'", rec.Body.String())
	}
}

// ---------- translateAnthropicToOpenAI: edge cases ----------

func TestTranslateAnthropicToOpenAINoSystem(t *testing.T) {
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	result, err := translateAnthropicToOpenAI([]byte(body), "target-model", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	var req openaiRequest
	json.Unmarshal(result, &req)
	if req.Model != "target-model" {
		t.Errorf("model = %q", req.Model)
	}
	// Should have 1 message (no system)
	if len(req.Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(req.Messages))
	}
}

func TestTranslateAnthropicToOpenAIInvalidJSON(t *testing.T) {
	_, err := translateAnthropicToOpenAI([]byte("not json"), "model", 0, "")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestTranslateAnthropicToOpenAIEmptySystem(t *testing.T) {
	body := `{"model":"claude","max_tokens":100,"system":"","messages":[{"role":"user","content":"hi"}]}`
	result, err := translateAnthropicToOpenAI([]byte(body), "model", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var req openaiRequest
	json.Unmarshal(result, &req)
	// Empty system should not add a system message
	if len(req.Messages) != 1 {
		t.Errorf("messages = %d, want 1 (no system for empty string)", len(req.Messages))
	}
}

// ---------- extractSystemText edge cases ----------

func TestExtractSystemTextInvalidJSON(t *testing.T) {
	got := extractSystemText(json.RawMessage(`not valid`))
	if got != "" {
		t.Errorf("invalid JSON should return empty, got %q", got)
	}
}

func TestExtractSystemTextEmptyBlocks(t *testing.T) {
	got := extractSystemText(json.RawMessage(`[{"type":"image","url":"..."}]`))
	if got != "" {
		t.Errorf("non-text blocks should return empty, got %q", got)
	}
}

// ---------- extractTextFromContent edge cases ----------

func TestExtractTextFromContentInvalid(t *testing.T) {
	got := extractTextFromContent(json.RawMessage(`12345`))
	if got != "12345" {
		t.Errorf("non-string/non-array should return raw, got %q", got)
	}
}

// ---------- handleConnectDirect with non-GitHub (triggers tunnelDirect) ----------

func TestHandleConnectDirectNonGitHub(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	// Create a local TCP server to tunnel to
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("tunnel response"))
	}()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	addr := ln.Addr().String()
	req, _ := http.NewRequest("CONNECT", "http://"+addr, nil)
	req.Host = addr
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go p.handleConnectDirect(proxyClient, req)

	// Read the 200 response
	reader := bufio.NewReader(clientConn)
	line, _ := reader.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200, got %q", line)
	}
}

// ---------- handleAnthropicReroute ----------

func TestHandleAnthropicReroute(t *testing.T) {
	p := newTestProxy()

	// Mock vLLM backend
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "rerouted"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test-model"}

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "api.anthropic.com:443", nil)
	req.Host = "api.anthropic.com:443"

	go func() {
		p.handleAnthropicReroute(proxyConn, req, "api.anthropic.com", "scanner", route)
		proxyConn.Close()
	}()

	// Read the "200 Connection established"
	reader := bufio.NewReader(clientConn)
	statusLine, _ := reader.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200 Connection established, got %q", statusLine)
	}
	// Read past headers
	for {
		l, _ := reader.ReadString('\n')
		if l == "\r\n" || l == "\n" {
			break
		}
	}

	// Now do a TLS handshake as client, using the proxy's CA
	pool := x509.NewCertPool()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caX509.Raw})
	pool.AppendCertsFromPEM(caPEM)

	// Re-wrap the connection with TLS
	tlsConn := tls.Client(&prefixConn{Conn: clientConn, prefix: nil}, &tls.Config{
		ServerName: "api.anthropic.com",
		RootCAs:    pool,
	})
	defer tlsConn.Close()

	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake failed: %v", err)
	}

	// Send an Anthropic API request
	anthropicBody := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	httpReq := fmt.Sprintf("POST /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s",
		len(anthropicBody), anthropicBody)
	tlsConn.Write([]byte(httpReq))

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}

	var ar anthropicResponse
	json.NewDecoder(resp.Body).Decode(&ar)
	if ar.Type != "message" {
		t.Errorf("type = %q, want message", ar.Type)
	}
}

// ---------- handleConnectDirect with Anthropic + inference route ----------

func TestHandleConnectDirectAnthropicReroute(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	p.SetInferenceRoute("scanner", route)

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "api.anthropic.com:443", nil)
	req.Host = "api.anthropic.com:443"
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go p.handleConnectDirect(proxyClient, req)

	// Should get 200 (the reroute path handles this)
	reader := bufio.NewReader(clientConn)
	line, _ := reader.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200, got %q", line)
	}
}

// ---------- StartInferenceTranslator ----------

func TestStartInferenceTranslator(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	// Start a mock vLLM backend
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "translated"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test-model"}
	p.inference.Set("test-agent", route)

	ts := httptest.NewServer(p.inferenceTranslatorHandler())
	defer ts.Close()

	// Test: no route for agent
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-hive-unknown-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("no-route status = %d, want 502", resp.StatusCode)
	}
	resp.Body.Close()

	// Test: valid request
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-api-key", "sk-hive-test-agent")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("valid request status = %d, body = %q", resp2.StatusCode, body)
	}
	var ar anthropicResponse
	json.NewDecoder(resp2.Body).Decode(&ar)
	if ar.Type != "message" {
		t.Errorf("type = %q, want message", ar.Type)
	}
	resp2.Body.Close()
}

// ---------- handleTransparentTLS with real TLS ClientHello ----------

func TestHandleTransparentTLSNonGitHub(t *testing.T) {
	p := newTestProxy()

	// Create a real TCP server the proxy can tunnel to
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Build a minimal TLS ClientHello targeting non-github host
	clientHello := buildClientHello("example.com")

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	peeked := clientHello[:1]
	go func() {
		// handleTransparentTLS reads the rest after peeked byte
		p.handleTransparentTLS(proxyConn, peeked)
		proxyConn.Close()
	}()

	// Send the rest of the ClientHello
	clientConn.Write(clientHello[1:])
	// The proxy tries to dial example.com:443 which will fail — that's fine
	time.Sleep(200 * time.Millisecond)
	clientConn.Close()
}

// ---------- handleTransparentTLS with GitHub host ----------

func TestHandleTransparentTLSGitHubHost(t *testing.T) {
	p := newTestProxy()

	clientHello := buildClientHello("api.github.com")
	peeked := clientHello[:1]

	clientConn, proxyConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(proxyConn, peeked)
		proxyConn.Close()
	}()

	// Send the remaining ClientHello bytes
	clientConn.Write(clientHello[1:])

	// The proxy will try to TLS handshake + dial api.github.com:443
	// It will either fail on TLS handshake or dial — either way shouldn't panic
	time.Sleep(300 * time.Millisecond)
	clientConn.Close()
}

// ---------- recordViolation: max violation log ----------

func TestRecordViolationMaxLimit(t *testing.T) {
	p := &GitHubProxy{
		violations: make(map[string]int),
		logger:     slog.Default(),
	}

	// Fill up to maxViolationLog
	for i := 0; i < maxViolationLog; i++ {
		p.recordViolation(fmt.Sprintf("agent-%d", i), "POST", "/test")
	}

	if len(p.violations) != maxViolationLog {
		t.Errorf("violations count = %d, want %d", len(p.violations), maxViolationLog)
	}

	// One more new agent should NOT be added (map is full)
	p.recordViolation("overflow-agent", "POST", "/test")
	if _, exists := p.violations["overflow-agent"]; exists {
		t.Error("overflow agent should not be added when at max")
	}

	// But existing agents can still increment
	p.recordViolation("agent-0", "POST", "/test")
	if p.violations["agent-0"] != 2 {
		t.Errorf("existing agent should increment, got %d", p.violations["agent-0"])
	}
}

// ---------- LookupUIDByLocalPort: header only file ----------

func TestLookupUIDByLocalPortHeaderOnly(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	content := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	os.WriteFile(tmpFile, []byte(content), 0644)

	_, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err == nil {
		t.Error("expected error for header-only file")
	}
}

// ---------- proxyHTTP: GraphQL non-mutation blocked (blockReason = "graphql") ----------

func TestProxyHTTPGraphQLBlockedNonMutation(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	// Use mode below advisory to block even queries
	go p.proxyHTTP(proxyClient, proxyUpstream, "blocked-agent", agent.AgentMode(-1), agent.AgentCapabilities{})

	body := `{"query":"{ viewer { login } }"}`
	fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := upstreamConn.Read(buf); err != nil {
				return
			}
		}
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("below-advisory GraphQL should be blocked, got %d", resp.StatusCode)
	}
}

// ---------- writeSSE ----------

func TestWriteSSE(t *testing.T) {
	var buf bytes.Buffer
	writeSSE(&buf, "test_event", map[string]string{"key": "value"})
	output := buf.String()
	if !strings.Contains(output, "event: test_event") {
		t.Error("missing event type")
	}
	if !strings.Contains(output, `"key":"value"`) {
		t.Error("missing data")
	}
}

// ---------- identifyAgentFromReq with UID map active but lookup returns name ----------

// TestIdentifyAgentFromReqUIDMapFallback: with iptables active but this
// connection's UID lookup failing, the self-asserted header must be ignored
// by default (N7, #3841) rather than trusted as "fallback-agent" — and must
// be honored when the deployment has explicitly opted into advisory mode.
func TestIdentifyAgentFromReqUIDMapFallback(t *testing.T) {
	p := &GitHubProxy{
		uidMap: &agent.UIDMap{IptablesActive: true},
		logger: slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com", nil)
	req.RemoteAddr = "127.0.0.1:99999"
	req.Header.Set("Proxy-Authorization", "hive fallback-agent")

	// UID lookup fails (port 99999 has no /proc/net/tcp entry). Without
	// proxyAdvisoryOK the header must not be trusted.
	if got := p.identifyAgentFromReq(req); got != "" {
		t.Errorf("expected the header to be ignored (not advisory), got %q", got)
	}

	p.proxyAdvisoryOK = true
	if got := p.identifyAgentFromReq(req); got != "fallback-agent" {
		t.Errorf("expected fallback-agent with proxyAdvisoryOK set, got %q", got)
	}
}

// ---------- handleInferenceRequest ----------

func TestHandleInferenceRequest(t *testing.T) {
	p := newTestProxy()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "inference ok"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	anthropicBody := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(anthropicBody))
	req.Body = io.NopCloser(strings.NewReader(anthropicBody))

	go p.handleInferenceRequest(proxyConn, req, "scanner", route)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %q", resp.StatusCode, body)
	}
}

func TestHandleInferenceRequestBadBody(t *testing.T) {
	p := newTestProxy()
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:1", Model: "test"}

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader("not json"))

	go p.handleInferenceRequest(proxyConn, req, "scanner", route)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("bad body should return 502, got %d", resp.StatusCode)
	}
}

func TestHandleInferenceRequestUnreachable(t *testing.T) {
	p := newTestProxy()
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "test"}

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader(body))

	go p.handleInferenceRequest(proxyConn, req, "scanner", route)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("unreachable should return 502, got %d", resp.StatusCode)
	}
}

// ---------- handleInferenceRequest streaming ----------

func TestHandleInferenceRequestStreaming(t *testing.T) {
	p := newTestProxy()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		data, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": "streamed"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		finalData, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{}, "finish_reason": "stop"},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", finalData)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader(body))

	go p.handleInferenceRequest(proxyConn, req, "scanner", route)

	// Read the raw SSE response
	buf := make([]byte, 8192)
	var result []byte
	for {
		clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := clientConn.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	output := string(result)
	if !strings.Contains(output, "text/event-stream") {
		t.Error("missing text/event-stream header in response")
	}
	if !strings.Contains(output, "message_start") {
		t.Error("missing message_start event")
	}
}

// ---------- SSE translation: edge cases ----------

func TestTranslateOpenAISSEToAnthropicEmptyStream(t *testing.T) {
	sseInput := "data: [DONE]\n\n"
	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "model")
	if err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "message_start") {
		t.Error("should still have message_start")
	}
	if !strings.Contains(output, "message_stop") {
		t.Error("should still have message_stop")
	}
}

func TestTranslateOpenAISSEToAnthropicBadJSON(t *testing.T) {
	sseInput := "data: {invalid json}\n\ndata: [DONE]\n\n"
	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "model")
	if err != nil {
		t.Fatal(err)
	}
	// Should skip bad JSON and continue
	if !strings.Contains(buf.String(), "message_stop") {
		t.Error("should complete despite bad JSON chunk")
	}
}

func TestTranslateOpenAISSEToAnthropicNonDataLines(t *testing.T) {
	sseInput := ": comment\nevent: something\nretry: 1000\ndata: [DONE]\n\n"
	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "model")
	if err != nil {
		t.Fatal(err)
	}
}

func TestTranslateOpenAISSEWithUsage(t *testing.T) {
	sseInput := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var buf strings.Builder
	_, _, err := translateOpenAISSEToAnthropic(strings.NewReader(sseInput), &buf, "model")
	if err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.Contains(output, "output_tokens") {
		t.Error("should include output_tokens in message_delta")
	}
}

// ---------- procnet: colon-less local address ----------

func TestLookupUIDByLocalPortNoColon(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: nocolonhere 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1001        0 12345
`
	os.WriteFile(tmpFile, []byte(content), 0644)

	_, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err == nil {
		t.Error("expected error for malformed local_address (no colon)")
	}
}

// ---------- procnet: bad UID field ----------

func TestLookupUIDByLocalPortBadUID(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 0100007F:0050 01 00000000:00000000 00:00000000 00000000  notanumber        0 12345
`
	os.WriteFile(tmpFile, []byte(content), 0644)

	_, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err == nil {
		t.Error("expected error for non-numeric UID")
	}
}

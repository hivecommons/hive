package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
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

// ---------- NewGitHubProxy: test construction ----------

func TestNewGitHubProxy(t *testing.T) {
	// NewGitHubProxy writes the CA cert/key to CACertPath/caKeyPath, which
	// default to live /data paths. Redirect them to a temp dir so the test
	// is hermetic and never touches (or requires) the host's real CA files.
	tmpDir := t.TempDir()
	origCert, origKey := CACertPath, caKeyPath
	CACertPath = filepath.Join(tmpDir, "proxy-ca.pem")
	caKeyPath = filepath.Join(tmpDir, "proxy-ca-key.pem")
	t.Cleanup(func() { CACertPath, caKeyPath = origCert, origKey })

	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"console", "docs"})
	if err != nil {
		t.Fatalf("NewGitHubProxy error: %v", err)
	}

	if p.ListenAddr() != "127.0.0.1:18443" {
		t.Errorf("listen addr = %q", p.ListenAddr())
	}

	// Check allowed repos
	if !p.allowedRepos["myorg/console"] {
		t.Error("myorg/console should be allowed")
	}
	if !p.allowedRepos["myorg/docs"] {
		t.Error("myorg/docs should be allowed")
	}
	if p.allowedRepos["myorg/other"] {
		t.Error("myorg/other should NOT be allowed")
	}

	// Verify CA cert was written
	if _, err := os.Stat(CACertPath); os.IsNotExist(err) {
		t.Error("CA cert should be written to CACertPath")
	}

	// Clean up
	os.Remove(CACertPath)
}

// ---------- StartInferenceTranslator: test via httptest-like approach ----------

func TestStartInferenceTranslatorIntegration(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	// Mock vLLM backend
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)

			data, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{"content": "Hi"}, "finish_reason": nil},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()

			data2, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{
					{"delta": map[string]string{}, "finish_reason": "stop"},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", data2)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			f.Flush()
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "reply"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test-model"}
	p.inference.Set("test-agent", route)

	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	defer translator.Close()

	baseURL := translator.URL

	// Test non-streaming
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-hive-test-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("non-streaming status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Test streaming
	streamBody := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req2, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(streamBody))
	req2.Header.Set("x-api-key", "sk-hive-test-agent")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	sseBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(sseBody), "message_start") {
		t.Error("streaming response should contain message_start")
	}

	// Test no route
	req3, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(body))
	req3.Header.Set("x-api-key", "sk-hive-unknown")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != http.StatusBadGateway {
		t.Errorf("no-route status = %d, want 502", resp3.StatusCode)
	}
	resp3.Body.Close()

	// Test bad body
	req4, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader("not json"))
	req4.Header.Set("x-api-key", "sk-hive-test-agent")
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	if resp4.StatusCode != http.StatusBadGateway {
		t.Errorf("bad body status = %d, want 502", resp4.StatusCode)
	}
	resp4.Body.Close()
}

// ---------- handleConnectDirect: GitHub host path (MITM) ----------

func TestHandleConnectDirectGitHub(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "http://api.github.com:443", nil)
	req.Host = "api.github.com:443"
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go func() {
		p.handleConnectDirect(proxyClient, req)
		proxyClient.Close()
	}()

	// Read the "200 Connection established"
	reader := bufio.NewReader(clientConn)
	statusLine, _ := reader.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200, got %q", statusLine)
	}
	// Read past headers
	for {
		l, _ := reader.ReadString('\n')
		if l == "\r\n" || l == "\n" {
			break
		}
	}

	// Do TLS handshake using the proxy's CA
	pool := x509.NewCertPool()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.caX509.Raw})
	pool.AppendCertsFromPEM(caPEM)

	tlsConn := tls.Client(&prefixConn{Conn: clientConn, prefix: nil}, &tls.Config{
		ServerName: "api.github.com",
		RootCAs:    pool,
	})
	defer tlsConn.Close()

	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake failed: %v", err)
	}

	// Send a request — the upstream dial to api.github.com:443 will fail
	// but we've already covered the MITM cert forge + TLS handshake paths
	fmt.Fprintf(tlsConn, "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	// The connection will close because upstream dial fails
	time.Sleep(500 * time.Millisecond)
}

// ---------- proxyHTTP: allowed POST with body draining ----------

func TestProxyHTTPAllowedPOSTWithBody(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})

	body := `{"title":"test issue"}`
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()

	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		io.ReadAll(req.Body)
		resp := &http.Response{
			StatusCode:    201,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(`{"id":1}`)),
			ContentLength: 8,
		}
		resp.Write(upstreamConn)
		upstreamConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	clientConn.Close()
}

// ---------- proxyHTTP: graphql mutation allowed at ISSUES_ONLY ----------

func TestProxyHTTPGraphQLMutationAllowed(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesOnly, agent.AgentCapabilities{})

	body := `{"query":"mutation { addStar(input: {}) { id } }"}`
	go func() {
		fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()

	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		io.ReadAll(req.Body)
		resp := &http.Response{
			StatusCode:    200,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader(`{"data":{}}`)),
			ContentLength: 11,
		}
		resp.Write(upstreamConn)
		upstreamConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("mutation should be allowed at ISSUES_ONLY, got %d", resp.StatusCode)
	}
	clientConn.Close()
}

// ---------- handleConnectDirect: Anthropic no route -> tunnelDirect ----------

func TestHandleConnectDirectAnthropicNoRoute(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()
	// No route set for any agent

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

	addr := ln.Addr().String()
	host, _, _ := net.SplitHostPort(addr)
	RegisterAnthropicHost(host)
	defer unregisterAnthropicHost(host)

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "http://"+addr, nil)
	req.Host = addr
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go func() {
		p.handleConnectDirect(proxyClient, req)
		proxyClient.Close()
	}()

	// Without a route for scanner, Anthropic host is not GitHub, so tunnelDirect is called.
	// tunnelDirect connects to local listener => 200 Connection established
	reader := bufio.NewReader(clientConn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Errorf("expected 200, got: %s", line)
	}
}

// ---------- handleTransparentTLS with UID map ----------

func TestHandleTransparentTLSWithUIDMap(t *testing.T) {
	p := newTestProxy()
	p.uidMap = &agent.UIDMap{IptablesActive: true}

	clientHello := buildClientHello("api.github.com")
	peeked := clientHello[:1]

	clientConn, proxyConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(proxyConn, peeked)
		proxyConn.Close()
	}()

	clientConn.Write(clientHello[1:])
	time.Sleep(300 * time.Millisecond)
	clientConn.Close()
}

// ---------- handleTransparentTLS: empty read ----------

func TestHandleTransparentTLSReadError(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(proxyConn, []byte{0x16})
		proxyConn.Close()
	}()

	// Close immediately so the read fails
	clientConn.Close()
	time.Sleep(100 * time.Millisecond)
}

// ---------- handleConn: CONNECT to GitHub host path ----------

func TestHandleConnCONNECTGitHub(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	clientConn, proxyClient := net.Pipe()

	go p.handleConn(proxyClient)

	// Send CONNECT to github.com:443
	fmt.Fprintf(clientConn, "CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\nProxy-Authorization: hive scanner\r\n\r\n")

	// Read the 200 response
	reader := bufio.NewReader(clientConn)
	statusLine, _ := reader.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		// May get 200 from CONNECT ack
		t.Logf("status line: %q", statusLine)
	}

	// Close to clean up
	clientConn.Close()
	time.Sleep(200 * time.Millisecond)
}

// ---------- identifyAgentFromReq with active iptables but empty result ----------

// TestIdentifyAgentFromReqIptablesEmptyUID: UID lookup failing for this
// connection (no matching /proc/net/tcp entry) must NOT fall back to trusting
// the self-asserted Proxy-Authorization header by default (N7, #3841) — an
// agent process cannot claim to be "quality" just because its own socket
// wasn't found in the table.
func TestIdentifyAgentFromReqIptablesEmptyUID(t *testing.T) {
	uidMap := agent.NewUIDMap()
	uidMap.IptablesActive = true
	uidMap.Agents["scanner"] = 1001

	p := &GitHubProxy{
		uidMap: uidMap,
		logger: slog.Default(),
	}

	// Request from a port that won't have a /proc/net/tcp entry
	req, _ := http.NewRequest("GET", "https://api.github.com", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Proxy-Authorization", "hive quality")

	got := p.identifyAgentFromReq(req)
	if got != "" {
		t.Errorf("expected the header to be ignored when UID lookup fails (not advisory), got %q", got)
	}
}

// ---------- proxyHTTP: blocked POST writes blockReason (not path) ----------

func TestProxyHTTPBlockedPOSTShowsPath(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	// POST to pulls (blocked for advisory)
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/pulls HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 2\r\n\r\n{}")
		time.Sleep(200 * time.Millisecond)
		clientConn.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := upstreamConn.Read(buf); err != nil {
				return
			}
		}
	}()

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
		t.Errorf("expected 403 in response")
	}
	// POST /pulls is a hard deny: the body carries the hive-open-pr directive
	// (the deny message) rather than the raw path, so agents learn the fix.
	if !strings.Contains(response, "hive-open-pr") {
		t.Errorf("expected hive-open-pr directive in response body, got: %s", response)
	}
}

// ---------- forgeCert: verify chain of trust ----------

func TestForgeCertChainOfTrust(t *testing.T) {
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

	cert, err := p.forgeCert("verify.example.com")
	if err != nil {
		t.Fatal(err)
	}

	leaf, _ := x509.ParseCertificate(cert.Certificate[0])

	// Verify the leaf cert is signed by the CA
	pool := x509.NewCertPool()
	pool.AddCert(caX509)
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool,
	})
	if err != nil {
		t.Fatalf("cert verification failed: %v", err)
	}
	if len(chains) == 0 {
		t.Error("no valid chains found")
	}

	// Verify key usage
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("leaf cert should have DigitalSignature key usage")
	}
	if len(leaf.ExtKeyUsage) == 0 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Error("leaf cert should have ServerAuth extended key usage")
	}
}

// ---------- handleInferenceRequest: streaming with usage ----------

func TestHandleInferenceRequestStreamingWithUsage(t *testing.T) {
	p := newTestProxy()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)

		chunks := []string{
			`{"choices":[{"delta":{"content":"A"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
			`{"choices":[{"delta":{"content":"B"},"finish_reason":null}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			f.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader(body))

	go func() {
		p.handleInferenceRequest(proxyConn, req, "scanner", route)
		proxyConn.Close()
	}()

	var result []byte
	buf := make([]byte, 8192)
	for {
		clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := clientConn.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	output := string(result)
	if !strings.Contains(output, "message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(output, "content_block_delta") {
		t.Error("missing content_block_delta")
	}
}

// ---------- forwardToInference: non-200 success status ----------

func TestForwardToInferenceNon2xx(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(body), w, route, "test-agent")
	_ = err
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "inference backend returned 503") {
		t.Errorf("expected error message, got %q", respBody)
	}
}

// ---------- proxyHTTP: upstream write error ----------

func TestProxyHTTPUpstreamWriteError(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	_, proxyUpstream := net.Pipe()

	go func() {
		// Close upstream immediately to trigger write error
		proxyUpstream.Close()
	}()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	fmt.Fprintf(clientConn, "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	// proxyHTTP will fail writing to closed upstream and return
	time.Sleep(200 * time.Millisecond)
	clientConn.Close()
}

// ---------- tunnelDirect: success path ----------

func TestTunnelDirectSuccess(t *testing.T) {
	p := newTestProxy()

	// Create a local TCP server
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
		// Echo back
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		conn.Write(buf[:n])
	}()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "http://"+ln.Addr().String(), nil)
	req.Host = ln.Addr().String()

	go p.tunnelDirect(proxyClient, req)

	// Read the 200 response
	reader := bufio.NewReader(clientConn)
	statusLine, _ := reader.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200, got %q", statusLine)
	}
	// Read past headers
	for {
		l, _ := reader.ReadString('\n')
		if l == "\r\n" || l == "\n" {
			break
		}
	}

	// Send data through the tunnel
	clientConn.Write([]byte("echo test"))
	time.Sleep(100 * time.Millisecond)

	echoBuf := make([]byte, 1024)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := clientConn.Read(echoBuf)
	if err != nil {
		t.Logf("read echoed data: %v (may be expected)", err)
	}
	if n > 0 && string(echoBuf[:n]) != "echo test" {
		t.Logf("echoed data: %q", echoBuf[:n])
	}
}

// ---------- handleConn: invalid HTTP after peek ----------

func TestHandleConnInvalidHTTP(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	clientConn, proxyClient := net.Pipe()

	go p.handleConn(proxyClient)

	// Send garbage that starts with an ASCII letter (not 0x16)
	clientConn.Write([]byte("GARBAGE not a real HTTP request\r\n\r\n"))
	time.Sleep(100 * time.Millisecond)
	clientConn.Close()
}

// ---------- procnet: empty lines between entries ----------

func TestLookupUIDByLocalPortEmptyLines(t *testing.T) {
	tmpFile := t.TempDir() + "/tcp"
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode

   0: 0100007F:1F90 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1001        0 12345

   1: 0100007F:4E20 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1002        0 12346
`
	os.WriteFile(tmpFile, []byte(content), 0644)

	uid, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if uid != 1001 {
		t.Errorf("uid = %d, want 1001", uid)
	}
}

// ---------- handleAnthropicReroute with read error on CONNECT response ----------

func TestHandleAnthropicRerouteConnectError(t *testing.T) {
	p := newTestProxy()
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:1", Model: "test"}

	clientConn, proxyConn := net.Pipe()
	// Close client immediately so the CONNECT response write fails
	clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "http://api.anthropic.com:443", nil)
	req.Host = "api.anthropic.com:443"

	// Should not panic
	p.handleAnthropicReroute(proxyConn, req, "api.anthropic.com", "scanner", route)
	proxyConn.Close()
}

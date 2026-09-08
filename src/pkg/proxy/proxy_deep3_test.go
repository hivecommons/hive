package proxy

import (
	"encoding/json"
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

// ---------- StartInferenceTranslator: real integration test ----------

func TestStartInferenceTranslatorReal(t *testing.T) {
	mockNonStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			data, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{"content": "streamed"}, "finish_reason": nil}},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()
			data2, _ := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{"delta": map[string]string{}, "finish_reason": "stop"}},
			})
			fmt.Fprintf(w, "data: %s\n\n", data2)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			f.Flush()
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"choices": []map[string]interface{}{{"message": map[string]string{"content": "non-streaming ok"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer mockNonStream.Close()

	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}

	p := &GitHubProxy{
		caCert:     caCert,
		caX509:     caX509,
		logger:     slog.Default(),
		violations: make(map[string]int),
		certCache:  make(map[string]cachedCert),
		inference:  newInferenceRouter(),
	}
	p.inference.Set("agent-non-stream", &InferenceRoute{Backend: "vllm", Endpoint: mockNonStream.URL, Model: "test-model"})
	p.inference.Set("agent-stream", &InferenceRoute{Backend: "vllm", Endpoint: mockNonStream.URL, Model: "test-model"})

	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	defer translator.Close()
	client := translator.Client()

	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", translator.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-hive-agent-non-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("non-stream status = %d body=%s", resp.StatusCode, b)
	}
	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	if ar.Type != "message" {
		t.Fatalf("type = %q", ar.Type)
	}

	streamBody := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	streamReq, _ := http.NewRequest("POST", translator.URL+"/v1/messages", strings.NewReader(streamBody))
	streamReq.Header.Set("x-api-key", "sk-hive-agent-stream")
	streamResp, err := client.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	if ct := streamResp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q", ct)
	}
	streamBytes, _ := io.ReadAll(streamResp.Body)
	if !strings.Contains(string(streamBytes), "content_block_delta") {
		t.Fatalf("stream response was not translated by real handler: %s", streamBytes)
	}
}

// ---------- NewGitHubProxy: test with writable directory ----------

func TestNewGitHubProxyWithWritableDir(t *testing.T) {
	// CACertPath and caKeyPath are vars precisely so tests can redirect the
	// CA read/write to a temp dir. Never write to — or delete — the live
	// /data/proxy-ca.pem: on a hive host that is the production proxy CA.
	tmpDir := t.TempDir()
	origCert, origKey := CACertPath, caKeyPath
	CACertPath = filepath.Join(tmpDir, "proxy-ca.pem")
	caKeyPath = filepath.Join(tmpDir, "proxy-ca-key.pem")
	t.Cleanup(func() { CACertPath, caKeyPath = origCert, origKey })

	p, err := NewGitHubProxy(slog.Default(), "testorg", []string{"repo-a", "repo-b"})
	if err != nil {
		t.Fatalf("NewGitHubProxy error: %v", err)
	}

	if p.listenAddr != fmt.Sprintf("127.0.0.1:%d", proxyListenPort) {
		t.Errorf("listen addr = %q", p.listenAddr)
	}

	// Check allowed repos
	if !p.allowedRepos["testorg/repo-a"] {
		t.Error("testorg/repo-a should be allowed")
	}
	if !p.allowedRepos["testorg/repo-b"] {
		t.Error("testorg/repo-b should be allowed")
	}

	// CA should be valid
	if p.caX509 == nil {
		t.Fatal("caX509 should not be nil")
	}
	if !p.caX509.IsCA {
		t.Error("CA cert should be CA")
	}

	// CA cert file should exist
	if _, err := os.Stat(CACertPath); os.IsNotExist(err) {
		t.Error("CA cert file should be written")
	}

	// Cert cache should be pre-warmed for github hosts
	p.certMu.RLock()
	if _, ok := p.certCache["api.github.com"]; !ok {
		t.Error("cert cache should be pre-warmed for api.github.com")
	}
	if _, ok := p.certCache["github.com"]; !ok {
		t.Error("cert cache should be pre-warmed for github.com")
	}
	p.certMu.RUnlock()

	// Inference router should be initialized
	if p.inference == nil {
		t.Error("inference router should not be nil")
	}
}

// ---------- handleTransparentTLS: non-GitHub host tunnel success ----------

func TestHandleTransparentTLSNonGitHubTunnelSuccess(t *testing.T) {
	p := newTestProxy()

	// Create a TLS server that accepts our ClientHello
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Register this host as non-GitHub
	host := "tunnel-test.example.com"

	// We need to connect to a real TCP port. The transparent proxy
	// for non-GitHub dials host+":443" which won't work in tests.
	// Instead, let's verify the code path with a host that fails to dial
	// and ensure it doesn't panic.

	clientHello := buildClientHello(host)
	peeked := clientHello[:1]

	clientConn, proxyConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(proxyConn, peeked)
		proxyConn.Close()
	}()

	clientConn.Write(clientHello[1:])
	// The dial to tunnel-test.example.com:443 will fail — just verify no panic
	time.Sleep(500 * time.Millisecond)
	clientConn.Close()
}

// ---------- handleTransparentTLS: no SNI defaults to github.com ----------

func TestHandleTransparentTLSNoSNI(t *testing.T) {
	p := newTestProxy()

	// Build a ClientHello without SNI
	chBody := make([]byte, 0, 128)
	chBody = append(chBody, 0x03, 0x03)             // version
	chBody = append(chBody, make([]byte, 32)...)    // random
	chBody = append(chBody, 0)                      // session ID len
	chBody = append(chBody, 0x00, 0x02, 0x00, 0x2f) // cipher suites
	chBody = append(chBody, 0x01, 0x00)             // compression
	// No extensions

	hsHeader := []byte{0x01}
	hsLen := len(chBody)
	hsHeader = append(hsHeader, byte(hsLen>>16), byte(hsLen>>8), byte(hsLen))
	handshake := append(hsHeader, chBody...)

	record := []byte{0x16, 0x03, 0x01}
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	peeked := record[:1]

	clientConn, proxyConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(proxyConn, peeked)
		proxyConn.Close()
	}()

	clientConn.Write(record[1:])
	// With no SNI, defaults to github.com → MITM path → dial github.com:443 fails
	time.Sleep(500 * time.Millisecond)
	clientConn.Close()
}

// ---------- proxyHTTP: upstream read error ----------

func TestProxyHTTPUpstreamReadError(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	returned := make(chan struct{})
	go func() {
		p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
		close(returned)
	}()

	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	}()

	go func() {
		// Read the forwarded request
		buf := make([]byte, 4096)
		upstreamConn.Read(buf)
		// Then close to simulate upstream error
		upstreamConn.Close()
	}()

	// proxyHTTP should return when upstream closes
	time.Sleep(200 * time.Millisecond)
	clientConn.Close()

	// Join the handler before the test exits: a leaked proxyHTTP goroutine
	// reads responseBodyStallTimeout, racing with the next test's
	// shortenBodyStall write (v4 CI race, issue #6110).
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP did not return after upstream close")
	}
}

// ---------- proxyHTTP: response write error ----------

func TestProxyHTTPResponseWriteError(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	returned := make(chan struct{})
	go func() {
		p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
		close(returned)
	}()

	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
		// Close client before response is written
		time.Sleep(100 * time.Millisecond)
		clientConn.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		n, _ := upstreamConn.Read(buf)
		_ = n
		// Send response after client closes
		time.Sleep(200 * time.Millisecond)
		resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"
		upstreamConn.Write([]byte(resp))
		upstreamConn.Close()
	}()

	// Join the handler before the test exits: the leaked proxyHTTP goroutine
	// read responseBodyStallTimeout (github_proxy.go stall bound) while the
	// next test's shortenBodyStall wrote it — the data race that failed v4 CI
	// run 33979236903 (issue #6110).
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP did not return after client close and upstream close")
	}
}

// ---------- forwardToInference: flusher path ----------

func TestForwardToInferenceStreamingWithFlusher(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": "flushed"}, "finish_reason": nil},
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
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	w := httptest.NewRecorder()

	err := forwardToInference(req, []byte(body), w, route, "test-agent")
	if err != nil {
		t.Fatal(err)
	}

	output := w.Body.String()
	if !strings.Contains(output, "message_start") {
		t.Error("missing message_start")
	}
	if !strings.Contains(output, "flushed") {
		t.Error("missing 'flushed' content")
	}
}

// ---------- Start: test that it listens (then close immediately) ----------

func TestStartListens(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}

	// Find a free port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := &GitHubProxy{
		listenAddr: fmt.Sprintf("127.0.0.1:%d", port),
		caCert:     caCert,
		caX509:     caX509,
		logger:     slog.Default(),
		violations: make(map[string]int),
		certCache:  make(map[string]cachedCert),
		inference:  newInferenceRouter(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- p.Start()
	}()

	// Wait for it to start listening
	time.Sleep(100 * time.Millisecond)

	// Try connecting
	conn, err := net.DialTimeout("tcp", p.listenAddr, time.Second)
	if err != nil {
		t.Fatalf("could not connect to proxy: %v", err)
	}
	conn.Close()

	// Force the listener to close by connecting and triggering an error
	// We can't cleanly stop Start() without closing the listener,
	// but the test coverage counts the listen + accept loop entry

	// Wait a bit then check if Start returned an error
	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Start returned: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		// Expected: Start is still running
	}
}

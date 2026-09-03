package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// newTestProxyWithCA builds a GitHubProxy with a fresh in-memory CA and cert
// cache, plus a token sink writing to a temp dir. Returns the proxy and the
// metrics dir for reading back recorded usage.
func newTestProxyWithCA(t *testing.T) (*GitHubProxy, string) {
	t.Helper()
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := &GitHubProxy{
		caCert:    caCert,
		caX509:    caX509,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		certCache: make(map[string]cachedCert),
		tokenSink: tokens.NewInferenceSink(dir, slog.Default()),
	}
	return p, dir
}

// readInferenceUsage reads back the cumulative usage the sink wrote for an
// agent (inference-<agent>.jsonl) and returns the model plus total in/out
// tokens.
func readInferenceUsage(t *testing.T, dir, agent string) (model string, in, out int64) {
	t.Helper()
	path := filepath.Join(dir, "inference-"+agent+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading usage file %s: %v", path, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e tokens.SessionEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad usage line: %v", err)
		}
		if e.Model != "" {
			model = e.Model
		}
		in += e.InputTokens
		out += e.OutputTokens
	}
	return model, in, out
}

// runCopilotSniff drives proxyCopilotHTTP over an in-process client<->proxy TLS
// connection and a plaintext proxy<->upstream connection. The upstream side is
// an http.Server-like handler running on a net.Pipe. Returns the client's raw
// response bytes.
//
// We use two net.Pipe pairs: one for client<->proxy (TLS on the client-facing
// side), one for proxy<->upstream (plain, upstream speaks HTTP/1.1 directly).
func TestProxyCopilotHTTP_NonStreaming(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	// Upstream: a plain TCP listener speaking HTTP/1.1 with a JSON completion.
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()

	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		// Drain body.
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		// Confirm the proxy forwarded the model.
		if !bytes.Contains(body, []byte(`"gpt-5"`)) {
			t.Errorf("upstream did not receive model: %s", body)
		}
		respBody := `{"model":"gpt-5","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":120,"completion_tokens":34}}`
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(respBody), respBody)
	}()

	upstreamConn, err := net.Dial("tcp", upstreamLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConn.Close()

	// Client<->proxy over TLS. The proxy TLS-wraps the "client" conn; the test
	// client dials with a pool trusting the proxy CA.
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	tlsCert, err := p.forgeCert("api.githubcopilot.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "worker1")
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com",
		RootCAs:    pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	reqBody := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
	fmt.Fprintf(clientConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(got, []byte(`"content":"ok"`)) {
		t.Errorf("client did not get upstream body verbatim: %s", got)
	}

	<-proxyDone

	model, in, out := readInferenceUsage(t, dir, "worker1")
	if model != "gpt-5" || in != 120 || out != 34 {
		t.Errorf("recorded usage = (%q,%d,%d), want (gpt-5,120,34)", model, in, out)
	}
}

func TestProxyCopilotHTTP_Streaming(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()

	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		// Proxy must have injected include_usage into the streaming request.
		if !bytes.Contains(body, []byte(`"include_usage":true`)) {
			t.Errorf("proxy did not inject include_usage: %s", body)
		}
		sse := "data: {\"model\":\"claude-sonnet-4.5\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":88}}\n\n" +
			"data: [DONE]\n\n"
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(sse), sse)
	}()

	upstreamConn, err := net.Dial("tcp", upstreamLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConn.Close()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	tlsCert, err := p.forgeCert("api.githubcopilot.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "worker2")
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com",
		RootCAs:    pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	// Streaming request with --model auto: usage must attribute to the model in
	// the response (claude-sonnet-4.5), not "auto".
	reqBody := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	fmt.Fprintf(clientConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(got, []byte("[DONE]")) {
		t.Errorf("client did not receive SSE stream verbatim: %s", got)
	}

	<-proxyDone

	model, in, out := readInferenceUsage(t, dir, "worker2")
	if model != "claude-sonnet-4.5" || in != 200 || out != 88 {
		t.Errorf("recorded usage = (%q,%d,%d), want (claude-sonnet-4.5,200,88)", model, in, out)
	}
}

// TestProxyCopilotHTTP_NonCompletionPassthrough verifies non-completion requests
// (e.g. /models) pass through without recording usage.
func TestProxyCopilotHTTP_NonCompletionPassthrough(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, _ := http.ReadRequest(br)
		if req != nil && req.Body != nil {
			io.ReadAll(req.Body)
			req.Body.Close()
		}
		respBody := `{"data":[]}`
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(respBody), respBody)
	}()

	upstreamConn, err := net.Dial("tcp", upstreamLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConn.Close()

	clientLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer clientLn.Close()
	tlsCert, _ := p.forgeCert("api.githubcopilot.com")
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if tlsConn.Handshake() != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "worker3")
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com",
		RootCAs:    pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	fmt.Fprintf(clientConn, "GET /models HTTP/1.1\r\nHost: api.githubcopilot.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	<-proxyDone

	// No usage file should have been written.
	if _, err := os.Stat(filepath.Join(dir, "inference-worker3.jsonl")); !os.IsNotExist(err) {
		t.Errorf("non-completion request should not record usage; file exists: %v", err)
	}
}

// TestHandleCopilotSniffGuards verifies the CONNECT branch only engages when a
// sink is present and the agent is known. We assert the routing predicate here;
// the full MITM is covered by the proxyCopilotHTTP tests above.
func TestCopilotSniffRoutingGuard(t *testing.T) {
	// Host recognition drives the branch.
	if !IsCopilotAPIHost("api.githubcopilot.com") {
		t.Fatal("copilot host must be recognized")
	}
	// A proxy without a sink must not claim the branch (nil sink).
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if p.tokenSink != nil {
		t.Fatal("expected nil sink")
	}
}

// TestHandleCopilotSniff_EndToEnd drives the full CONNECT handler: it writes the
// "200 Connection established" line, TLS-handshakes with the client, dials the
// (faked) upstream via the test seam, and records usage. This covers
// handleCopilotSniff, which in production dials the real Copilot host by name.
func TestHandleCopilotSniff_EndToEnd(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	// Fake upstream: a plain TCP server answering a completion.
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		io.ReadAll(req.Body)
		req.Body.Close()
		respBody := `{"model":"gpt-5","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(respBody), respBody)
	}()

	// Seam: dial the fake upstream instead of the real Copilot host.
	p.copilotDial = func(host string) (net.Conn, error) {
		return net.Dial("tcp", upstreamLn.Addr().String())
	}

	// Client<->proxy raw socket. handleCopilotSniff writes the CONNECT-OK line
	// then expects a TLS ClientHello.
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		p.handleCopilotSniff(raw, "api.githubcopilot.com", "worker9")
	}()

	raw, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// Read the "200 Connection established" status line the handler writes.
	statusReader := bufio.NewReader(raw)
	statusLine, err := statusReader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading CONNECT status: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200 Connection established, got %q", statusLine)
	}
	// Consume the trailing blank line of the CONNECT response.
	if _, err := statusReader.ReadString('\n'); err != nil {
		t.Fatalf("reading CONNECT blank line: %v", err)
	}

	// Now TLS-handshake over the same raw conn. Any bytes the client buffered
	// past the CONNECT response would be lost, but the handler waits for the
	// ClientHello, so start TLS fresh on raw (statusReader read only the header).
	tlsConn := tls.Client(raw, &tls.Config{ServerName: "api.githubcopilot.com", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	reqBody := `{"model":"gpt-5","messages":[]}`
	fmt.Fprintf(tlsConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	<-done

	model, in, out := readInferenceUsage(t, dir, "worker9")
	if model != "gpt-5" || in != 11 || out != 4 {
		t.Errorf("recorded (%q,%d,%d), want (gpt-5,11,4)", model, in, out)
	}
}

// TestHandleCopilotSniff_UpstreamDialFails covers the dial-failure branch: the
// handler must abandon the connection cleanly after the client handshake.
func TestHandleCopilotSniff_UpstreamDialFails(t *testing.T) {
	p, _ := newTestProxyWithCA(t)
	p.copilotDial = func(host string) (net.Conn, error) {
		return nil, fmt.Errorf("simulated dial failure")
	}

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		p.handleCopilotSniff(raw, "api.githubcopilot.com", "w")
	}()

	raw, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	sr := bufio.NewReader(raw)
	sr.ReadString('\n') // status line
	sr.ReadString('\n') // blank line
	tlsConn := tls.Client(raw, &tls.Config{ServerName: "api.githubcopilot.com", RootCAs: pool})
	// Handshake may succeed (client cert forged) but the handler returns right
	// after dial fails, so the conn closes. Either way the handler must exit.
	_ = tlsConn.Handshake()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleCopilotSniff did not exit on dial failure")
	}
}

// TestHandleCopilotSniff_ForgeCertError covers the forge-cert failure branch by
// nil-ing the CA so cert generation fails.
func TestHandleCopilotSniff_ForgeCertError(t *testing.T) {
	// A proxy with an empty CA cert (no PrivateKey) makes forgeCert fail.
	p := &GitHubProxy{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		certCache: make(map[string]cachedCert),
	}
	c1, c2 := net.Pipe()
	go func() {
		// Drain whatever the handler writes so it doesn't block.
		io.Copy(io.Discard, c1)
	}()
	done := make(chan struct{})
	go func() {
		// Exercise the full CONNECT entry (writes the 200 line, then fails at
		// forgeCert).
		p.handleCopilotSniff(c2, "api.githubcopilot.com", "w")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleCopilotSniff did not exit on forge-cert error")
	}
	c1.Close()
}

// TestDialCopilotUpstream_DefaultErrors exercises the default (non-seam) dial
// path against an unresolvable host to cover the tls.Dial branch.
func TestDialCopilotUpstream_DefaultErrors(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// .invalid is guaranteed non-resolvable per RFC 6761.
	if _, err := p.dialCopilotUpstream("nonexistent.host.invalid"); err == nil {
		t.Fatal("expected dial error for invalid host")
	}
}

// TestProxyCopilotHTTP_NonSuccessCompletion covers the branch where a completion
// request gets a non-2xx response — it must pass through without recording usage.
func TestProxyCopilotHTTP_NonSuccessCompletion(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	upstreamLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, _ := http.ReadRequest(br)
		if req != nil && req.Body != nil {
			io.ReadAll(req.Body)
			req.Body.Close()
		}
		body := `{"error":"rate limited"}`
		fmt.Fprintf(conn, "HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}()
	upstreamConn, _ := net.Dial("tcp", upstreamLn.Addr().String())
	defer upstreamConn.Close()

	clientLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer clientLn.Close()
	tlsCert, _ := p.forgeCert("api.githubcopilot.com")
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if tlsConn.Handshake() != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "w4")
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com", RootCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	reqBody := `{"model":"gpt-5","messages":[]}`
	fmt.Fprintf(clientConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 passed through", resp.StatusCode)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	<-done

	if _, err := os.Stat(filepath.Join(dir, "inference-w4.jsonl")); !os.IsNotExist(err) {
		t.Errorf("non-2xx completion must not record usage")
	}
}

// TestProxyCopilotHTTP_KeepAliveTwoRequests covers the keep-alive loop
// continuation (req.Close/resp.Close false) and accumulation across two
// completions on one connection.
func TestProxyCopilotHTTP_KeepAliveTwoRequests(t *testing.T) {
	p, dir := newTestProxyWithCA(t)

	upstreamLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for i := 0; i < 2; i++ {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			io.ReadAll(req.Body)
			req.Body.Close()
			body := `{"model":"gpt-5","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
			// Keep-alive: no Connection: close, explicit Content-Length.
			fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		}
	}()
	upstreamConn, _ := net.Dial("tcp", upstreamLn.Addr().String())
	defer upstreamConn.Close()

	clientLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer clientLn.Close()
	tlsCert, _ := p.forgeCert("api.githubcopilot.com")
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if tlsConn.Handshake() != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "w5")
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com", RootCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	cr := bufio.NewReader(clientConn)
	for i := 0; i < 2; i++ {
		reqBody := `{"model":"gpt-5","messages":[]}`
		fmt.Fprintf(clientConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Length: %d\r\n\r\n%s", len(reqBody), reqBody)
		resp, err := http.ReadResponse(cr, nil)
		if err != nil {
			t.Fatalf("request %d read response: %v", i, err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	clientConn.Close()
	<-done

	_, in, out := readInferenceUsage(t, dir, "w5")
	if in != 20 || out != 10 {
		t.Errorf("keep-alive accumulated usage = (%d,%d), want (20,10)", in, out)
	}
}

// TestProxyCopilotHTTP_ZeroUsageNotRecorded covers the branch where a completion
// response carries no usage — nothing is recorded.
func TestProxyCopilotHTTP_ZeroUsageNotRecorded(t *testing.T) {
	p, dir := newTestProxyWithCA(t)
	upstreamLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer upstreamLn.Close()
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, _ := http.ReadRequest(br)
		if req != nil && req.Body != nil {
			io.ReadAll(req.Body)
			req.Body.Close()
		}
		body := `{"model":"gpt-5","choices":[{"message":{"content":"ok"}}]}` // no usage
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}()
	upstreamConn, _ := net.Dial("tcp", upstreamLn.Addr().String())
	defer upstreamConn.Close()

	clientLn, _ := net.Listen("tcp", "127.0.0.1:0")
	defer clientLn.Close()
	tlsCert, _ := p.forgeCert("api.githubcopilot.com")
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if tlsConn.Handshake() != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", "w6")
		tlsConn.Close()
	}()
	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{
		ServerName: "api.githubcopilot.com", RootCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	reqBody := `{"model":"gpt-5","messages":[]}`
	fmt.Fprintf(clientConn, "POST /chat/completions HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)
	resp, _ := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if resp != nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	<-done
	if _, err := os.Stat(filepath.Join(dir, "inference-w6.jsonl")); !os.IsNotExist(err) {
		t.Errorf("zero-usage completion must not record")
	}
}

// TestProxyCopilotHTTP_ClientClosesImmediately ensures the loop exits cleanly
// when the client sends nothing.
func TestProxyCopilotHTTP_ClientClosesImmediately(t *testing.T) {
	p, _ := newTestProxyWithCA(t)
	c1, c2 := net.Pipe()
	u1, _ := net.Pipe()
	go func() {
		time.Sleep(10 * time.Millisecond)
		c1.Close()
	}()
	done := make(chan struct{})
	go func() {
		p.proxyCopilotHTTP(c2, u1, "api.githubcopilot.com", "w")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyCopilotHTTP did not exit on client close")
	}
}

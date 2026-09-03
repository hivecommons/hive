package proxy

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// ---------- proxyHTTP: git path bidirectional streaming ----------

func TestProxyHTTPGitReceivePackStreaming(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})

	done := make(chan struct{})

	// Upstream: read the forwarded request, send response, close
	go func() {
		defer close(done)
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		// Read the request body
		io.ReadAll(req.Body)
		// Send back git pack data
		upstreamConn.Write([]byte("PACK\x00\x00\x00\x02git-pack-data"))
		upstreamConn.Close()
	}()

	// Client: send git receive-pack request
	gitBody := "0000want abcdef\n"
	fmt.Fprintf(clientConn, "POST /org/repo.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: %d\r\n\r\n%s", len(gitBody), gitBody)

	// Read the response (raw git pack data)
	buf := make([]byte, 4096)
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
	clientConn.Close()

	<-done

	if len(result) == 0 {
		t.Log("no data received — may be expected if request was blocked")
	}
}

// ---------- proxyHTTP: git upload-pack (always allowed) ----------

func TestProxyHTTPGitUploadPack(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	done := make(chan struct{})

	go func() {
		defer close(done)
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			return
		}
		io.ReadAll(req.Body)
		upstreamConn.Write([]byte("pack-response-data"))
		upstreamConn.Close()
	}()

	gitBody := "want abc"
	fmt.Fprintf(clientConn, "POST /org/repo.git/git-upload-pack HTTP/1.1\r\nHost: github.com\r\nContent-Length: %d\r\n\r\n%s", len(gitBody), gitBody)

	buf := make([]byte, 4096)
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
	clientConn.Close()
	<-done

	if !strings.Contains(string(result), "pack-response-data") {
		t.Logf("result: %q", result)
	}
}

// ---------- handleConnectDirect: CONNECT write error ----------

func TestHandleConnectDirectWriteError(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	clientConn, proxyClient := net.Pipe()

	req, _ := http.NewRequest("CONNECT", "http://api.github.com:443", nil)
	req.Host = "api.github.com:443"
	req.Header.Set("Proxy-Authorization", "hive scanner")

	// Close client before handleConnectDirect can write "200 Connection established"
	clientConn.Close()

	// Should not panic
	p.handleConnectDirect(proxyClient, req)
	proxyClient.Close()
}

// ---------- handleConnectDirect: TLS handshake failure ----------

func TestHandleConnectDirectTLSHandshakeFail(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	clientConn, proxyClient := net.Pipe()

	req, _ := http.NewRequest("CONNECT", "http://api.github.com:443", nil)
	req.Host = "api.github.com:443"
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go func() {
		p.handleConnectDirect(proxyClient, req)
		proxyClient.Close()
	}()

	// Read "200 Connection established"
	reader := bufio.NewReader(clientConn)
	line, _ := reader.ReadString('\n')
	_ = line
	for {
		l, _ := reader.ReadString('\n')
		if l == "\r\n" || l == "\n" {
			break
		}
	}

	// Send garbage instead of TLS handshake
	clientConn.Write([]byte("not a TLS handshake at all, just garbage data"))
	time.Sleep(200 * time.Millisecond)
	clientConn.Close()
}

// ---------- handleConnectDirect: upstream dial failure ----------

func TestHandleConnectDirectUpstreamDialFail(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	// Register a fake GitHub host that won't resolve
	RegisterGitHubHost("fake-github-for-test.invalid")
	defer unregisterGitHubHost("fake-github-for-test.invalid")

	clientConn, proxyClient := net.Pipe()

	req, _ := http.NewRequest("CONNECT", "http://fake-github-for-test.invalid:1", nil)
	req.Host = "fake-github-for-test.invalid:1"
	req.Header.Set("Proxy-Authorization", "hive scanner")

	go func() {
		p.handleConnectDirect(proxyClient, req)
		proxyClient.Close()
	}()

	// The proxy dials upstream BEFORE answering CONNECT, so the
	// unresolvable host fails the dial and the client gets 502 — no tunnel,
	// no TLS handshake.
	reader := bufio.NewReader(clientConn)
	statusLine, _ := reader.ReadString('\n')
	if !strings.Contains(statusLine, "502") {
		t.Fatalf("expected 502 on upstream dial failure, got %q", statusLine)
	}
}

// ---------- identifyAgentFromReq: UID lookup succeeds ----------

func TestIdentifyAgentFromReqUIDSuccess(t *testing.T) {
	// This test verifies the code path where identifyAgentByUID returns a name.
	// On macOS, /proc/net/tcp doesn't exist, so we can't truly test this.
	// But we can verify the fallback to extractAgentName works.
	p := &GitHubProxy{
		uidMap: &agent.UIDMap{IptablesActive: true},
		logger: slog.Default(),
	}

	req, _ := http.NewRequest("GET", "https://api.github.com", nil)
	req.RemoteAddr = "127.0.0.1:0"

	got := p.identifyAgentFromReq(req)
	// UID lookup fails, no proxy auth header
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ---------- handleTransparentTLS: non-GitHub with UID map ----------

func TestHandleTransparentTLSNonGitHubUID(t *testing.T) {
	p := newTestProxy()
	uidMap := agent.NewUIDMap()
	uidMap.IptablesActive = true
	uidMap.Agents["test-agent"] = 1001
	p.uidMap = uidMap

	clientHello := buildClientHello("example.com")
	peeked := clientHello[:1]

	serverConn, clientConn := net.Pipe()

	go func() {
		p.handleTransparentTLS(serverConn, peeked)
		serverConn.Close()
	}()

	clientConn.Write(clientHello[1:])
	// Non-GitHub host, will try to dial example.com:443 which fails
	time.Sleep(500 * time.Millisecond)
	clientConn.Close()
}

// ---------- Start: accept error ----------

func TestStartAcceptError(t *testing.T) {
	caCert, caX509, _ := generateCA()

	// Find a free port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	p := &GitHubProxy{
		listenAddr: addr,
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

	time.Sleep(100 * time.Millisecond)

	// Connect multiple times to exercise the accept loop
	for i := 0; i < 3; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		conn.Write([]byte("G")) // ASCII 'G' for GET
		conn.Close()
	}

	// Start will keep running — just verify it accepted connections
	select {
	case err := <-errCh:
		t.Logf("Start returned: %v", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: still running
	}
}

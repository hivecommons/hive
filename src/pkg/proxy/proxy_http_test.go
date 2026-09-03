package proxy

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func newTestProxy() *GitHubProxy {
	caCert, caX509, _ := generateCA()
	return &GitHubProxy{
		caCert:     caCert,
		caX509:     caX509,
		logger:     slog.Default(),
		violations: make(map[string]int),
		certCache:  make(map[string]cachedCert),
	}
}

func TestProxyHTTPAllowedGET(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

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
		upstreamConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("GET should be allowed, got %d", resp.StatusCode)
	}
}

func TestProxyHTTPBlockedPOST(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	// Write request synchronously — blocked POST gets 403 back on same conn
	fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 2\r\n\r\n{}")

	// Drain upstream in background to prevent hang
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := upstreamConn.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST issues should be blocked for advisory, got %d", resp.StatusCode)
	}

	if p.AgentViolations("scanner") != 1 {
		t.Errorf("should record 1 violation, got %d", p.AgentViolations("scanner"))
	}
}

func TestProxyHTTPGraphQLMutationBlocked(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	body := `{"query":"mutation { addStar(input: {}) { id } }"}`
	go func() {
		fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
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

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GraphQL mutation should be blocked, got %d", resp.StatusCode)
	}
}

func TestProxyHTTPGraphQLQueryAllowed(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	body := `{"query":"{ viewer { login } }"}`
	go func() {
		fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
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
		upstreamConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("GraphQL query should be allowed, got %d", resp.StatusCode)
	}
}

func TestProxyHTTPClientClose(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	_, proxyUpstream := net.Pipe()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})

	clientConn.Close()
}

func TestForwardPlainDirectBadUpstream(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("GET", "http://nonexistent-host-xyz.invalid/path", nil)
	go p.forwardPlainDirect(proxyClient, req)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 502 {
		t.Errorf("bad upstream should return 502, got %d", resp.StatusCode)
	}
}

func TestHandleConnClientClose(t *testing.T) {
	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	clientConn.Close()
	p.handleConn(proxyClient)
}

func TestExtractSNIFromRealHandshake(t *testing.T) {
	data := make([]byte, 200)
	data[0] = 0x16 // TLS handshake
	data[1] = 0x03
	data[2] = 0x01
	data[3] = 0x00
	data[4] = byte(len(data) - 5)
	data[5] = 0x01 // ClientHello

	sni := extractSNI(data)
	if sni != "" {
		t.Logf("extracted SNI: %q (may be empty for minimal handshake)", sni)
	}
}

func TestIdentifyAgentFromReqWithUIDMap(t *testing.T) {
	p := newTestProxy()
	p.uidMap = &agent.UIDMap{IptablesActive: true}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	got := p.identifyAgentFromReq(req)
	// UID lookup will fail (no /proc/net/tcp on macOS) — falls back to extractAgentName
	if got != "" {
		t.Logf("agent name: %q", got)
	}
}

func TestTunnelDirectBadHost(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("CONNECT", "nonexistent-host-xyz.invalid:443", nil)
	req.Host = "nonexistent-host-xyz.invalid:443"

	go p.tunnelDirect(proxyClient, req)

	resp := make([]byte, 4096)
	n, _ := clientConn.Read(resp)
	response := string(resp[:n])
	if !strings.Contains(response, "502") {
		t.Errorf("bad host should return 502, got: %s", response)
	}
}

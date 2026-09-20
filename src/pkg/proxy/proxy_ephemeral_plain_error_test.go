package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestNewGitHubProxyEphemeral covers the ephemeral constructor: a fresh
// in-memory CA (never persisted to /data) wired through the same
// newGitHubProxyWithCA body as the production constructor. It must produce a
// proxy whose write allowlist and listen address match NewGitHubProxy's.
func TestNewGitHubProxyEphemeral(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	p, err := NewGitHubProxyEphemeral(logger, "hivecommons", []string{"hive", "console"})
	if err != nil {
		t.Fatalf("NewGitHubProxyEphemeral: %v", err)
	}

	if p.caX509 == nil {
		t.Error("caX509 should be populated from the generated CA")
	}
	if p.caCert.PrivateKey == nil {
		t.Error("caCert should carry the CA private key")
	}
	if !p.caX509.IsCA {
		t.Error("generated certificate should be a CA")
	}

	for _, repo := range []string{"hivecommons/hive", "hivecommons/console"} {
		if !p.allowedRepos[repo] {
			t.Errorf("allowedRepos missing %q", repo)
		}
	}
	if len(p.allowedRepos) != 2 {
		t.Errorf("allowedRepos = %v, want exactly the 2 org/repo keys", p.allowedRepos)
	}

	if got := p.ListenAddr(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("ListenAddr() = %q, want 127.0.0.1:<port>", got)
	}

	// The routing/provider state the cmd/hive boot tests rely on must be
	// initialised, not left nil.
	if p.inference == nil || p.entitlements == nil || p.gatewayHealth == nil {
		t.Error("inference router, entitlement store and gateway health store must be initialised")
	}
	if p.violations == nil || p.certCache == nil {
		t.Error("violations and certCache maps must be initialised")
	}
}

// TestForwardPlainDirectUpstreamError covers the RoundTrip failure branch:
// when the upstream is unreachable the agent must receive an HTTP 502, not a
// hung or silently closed connection.
func TestForwardPlainDirectUpstreamError(t *testing.T) {
	// Reserve a port and close the listener so the dial deterministically
	// fails with connection refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()

	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	defer clientConn.Close()

	req, err := http.NewRequest("GET", "http://"+deadAddr+"/test", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	go func() {
		p.forwardPlainDirect(proxyClient, req)
		// forwardPlainDirect leaves the conn open (handleConn owns its
		// lifecycle in production); close it here so the body read below
		// sees EOF instead of waiting out the deadline.
		_ = proxyClient.Close()
	}()

	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream request failed") {
		t.Errorf("body = %q, want upstream failure message", string(body))
	}
}

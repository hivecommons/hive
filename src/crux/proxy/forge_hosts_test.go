package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

const testForgeHost = "forgejo.example.test"

func withForgeHosts(t *testing.T, hosts ...string) {
	t.Helper()
	SetForgeHosts(hosts)
	t.Cleanup(func() { SetForgeHosts(nil) })
}

// A GitHub-only hive registers no forge host, so nothing it did not inspect
// before is inspected now.
func TestForgeHostsEmptyByDefault(t *testing.T) {
	withForgeHosts(t)
	for _, h := range []string{testForgeHost, "codeberg.org", "example.com", ""} {
		if IsForgeHost(h) || NeedsInspection(h) || NeedsMITM(h) {
			t.Fatalf("%q is inspected with no forge configured", h)
		}
	}
	if !NeedsInspection("api.github.com") || NeedsInspection("github.com") {
		t.Fatal("GitHub inspection changed")
	}
}

func TestRegisterForgeHost(t *testing.T) {
	withForgeHosts(t)
	RegisterForgeHost(" Forgejo.Example.Test. ")
	RegisterForgeHost("")
	if !IsForgeHost(testForgeHost) || !NeedsInspection(testForgeHost) {
		t.Fatal("registered forge host is not inspected")
	}
	if IsForgeHost("other.example.test") || IsForgeHost("") {
		t.Fatal("an unregistered host counts as a forge host")
	}
	// Only the configured host: a subdomain or a suffix is not it.
	for _, h := range []string{"evil." + testForgeHost, testForgeHost + ".evil.example"} {
		if IsForgeHost(h) {
			t.Fatalf("%q matched the forge host", h)
		}
	}
}

// A forge host can never become, or shadow, a GitHub host.
func TestForgeHostIsNeverAGitHubHost(t *testing.T) {
	withForgeHosts(t, testForgeHost, "github.com", "api.github.com")
	if IsForgeHost("github.com") || IsForgeHost("api.github.com") {
		t.Fatal("a GitHub host was registered as a forge host")
	}
	RegisterForgeHost("api.github.com")
	if IsForgeHost("api.github.com") {
		t.Fatal("RegisterForgeHost accepted a GitHub host")
	}
	if IsGitHubHost(testForgeHost) || injectsGitHubAuthForHost(testForgeHost) {
		t.Fatal("a forge host is treated as GitHub for credential injection")
	}
}

func TestSetForgeHostsReplaces(t *testing.T) {
	withForgeHosts(t, testForgeHost)
	SetForgeHosts([]string{"other.example.test"})
	if IsForgeHost(testForgeHost) {
		t.Fatal("a removed forge host is still inspected")
	}
	if !IsForgeHost("other.example.test") {
		t.Fatal("the new forge host is not inspected")
	}
}

// relayForgeRequest pushes one agent request for host through the real
// proxyHTTPHost and returns the status the agent got and the raw bytes the
// upstream received ("" when nothing was forwarded).
func relayForgeRequest(t *testing.T, p *GitHubProxy, host string, mode agent.AgentMode, rawReq string) (int, string) {
	t.Helper()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	done := make(chan struct{})
	go func() {
		p.proxyHTTPHost(proxyClient, proxyUpstream, host, testAgentName, mode, agent.AgentCapabilities{})
		close(done)
	}()
	captured := make(chan string, 1)
	go func() {
		var raw bytes.Buffer
		req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(upstreamConn, &raw)))
		if err != nil {
			captured <- ""
			return
		}
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
		}
		captured <- raw.String()
		fmt.Fprintf(upstreamConn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}()
	go func() { _, _ = io.WriteString(clientConn, rawReq) }()
	_ = clientConn.SetReadDeadline(time.Now().Add(exchangeTimeout))
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	clientConn.Close()
	upstreamConn.Close()
	<-done
	var raw string
	select {
	case raw = <-captured:
	case <-time.After(exchangeTimeout):
		t.Fatal("timed out waiting for the upstream reader")
	}
	return resp.StatusCode, raw
}

// With injection on, a request to a forge host still carries no GitHub token:
// the agent's own header is forwarded as it was, and the token source is
// never consulted for it.
func TestForgeHostRequestNeverGetsAGitHubToken(t *testing.T) {
	withForgeHosts(t, testForgeHost)
	var calls []string
	p := injectionTestProxy(&calls)
	status, raw := relayForgeRequest(t, p, testForgeHost, agent.ModeAdvisory,
		"GET /api/v1/repos/team/app HTTP/1.1\r\nHost: "+testForgeHost+"\r\n\r\n")
	if status != http.StatusOK {
		t.Fatalf("read on the forge = %d, want it forwarded", status)
	}
	if strings.Contains(raw, testScopedToken) || strings.Contains(strings.ToLower(raw), "authorization:") {
		t.Fatalf("forge request carried a credential the agent did not send:\n%s", raw)
	}
	if len(calls) != 0 {
		t.Fatalf("token source consulted for a forge host: %v", calls)
	}
}

// The Gitea/Forgejo hard denies now run for a configured forge host: an
// agent's direct PR creation or merge never reaches the forge.
func TestForgeHostHardDeniesApply(t *testing.T) {
	withForgeHosts(t, testForgeHost)
	p := injectionTestProxy(nil)
	for _, line := range []string{
		"POST /api/v1/repos/team/app/pulls",
		"POST /api/v1/repos/team/app/pulls/3/merge",
	} {
		status, raw := relayForgeRequest(t, p, testForgeHost, agent.ModeIssuesPRsMerge,
			line+" HTTP/1.1\r\nHost: "+testForgeHost+"\r\nContent-Length: 2\r\n\r\n{}")
		if status != http.StatusForbidden {
			t.Fatalf("%s = %d, want 403", line, status)
		}
		if raw != "" {
			t.Fatalf("%s reached the forge:\n%s", line, raw)
		}
	}
}

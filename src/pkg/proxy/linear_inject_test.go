package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// relayLinearRequest pushes one agent request for api.linear.app through the
// real proxyHTTPHost over net.Pipes and returns the request as the upstream
// (Linear) would receive it. authHeader is what the AGENT put on the wire —
// the stale copy of a rotated token, a personal key, or nothing.
func relayLinearRequest(t *testing.T, p *GitHubProxy, mode agent.AgentMode, authHeader string) *http.Request {
	t.Helper()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTPHost(proxyClient, proxyUpstream, "api.linear.app", "quality", mode, agent.AgentCapabilities{})

	body := `{"query":"{ viewer { id } }"}`
	go func() {
		auth := ""
		if authHeader != "" {
			auth = "Authorization: " + authHeader + "\r\n"
		}
		fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.linear.app\r\nContent-Type: application/json\r\n%sContent-Length: %d\r\n\r\n%s", auth, len(body), body)
	}()

	forwarded := make(chan *http.Request, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			forwarded <- nil
			return
		}
		forwarded <- req
		resp := &http.Response{StatusCode: 200, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: http.NoBody, Request: req}
		_ = resp.Write(upstreamConn)
		upstreamConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("request should have been forwarded, got %d", resp.StatusCode)
	}
	req := <-forwarded
	if req == nil {
		t.Fatal("upstream never received the request")
	}
	return req
}

// A rotated OAuth token in an agent's process is exactly what the live hive
// showed: the session env held the fresh token, the CLI's env the dead one,
// and every Linear call 401'd for two days. With the resolver wired, Linear
// must see the hive's CURRENT token regardless of what the agent sent.
func TestLinearInject_ReplacesStaleAgentTokenWithCurrent(t *testing.T) {
	p := newTestProxy()
	p.SetLinearCredentialResolver(func() agent.LinearCredential { return agent.LinearCredential{AccessToken: "lin_oauth_current"} })

	req := relayLinearRequest(t, p, agent.ModeIssuesOnly, "Bearer lin_oauth_rotated_out")
	if got := req.Header.Get("Authorization"); got != "Bearer lin_oauth_current" {
		t.Fatalf("Linear must receive the hive's current token, got %q", got)
	}
	if vals := req.Header.Values("Authorization"); len(vals) != 1 {
		t.Fatalf("exactly one Authorization header must reach Linear, got %v", vals)
	}
}

// The agent's own copy is irrelevant either way: with NO header at all the
// proxy still authenticates the request.
func TestLinearInject_AttachesCredentialWhenAgentSentNone(t *testing.T) {
	p := newTestProxy()
	p.SetLinearCredentialResolver(func() agent.LinearCredential { return agent.LinearCredential{AccessToken: "lin_oauth_current"} })

	req := relayLinearRequest(t, p, agent.ModeIssuesAndPRs, "")
	if got := req.Header.Get("Authorization"); got != "Bearer lin_oauth_current" {
		t.Fatalf("Linear must receive the hive's current token, got %q", got)
	}
}

// Without a connected app the work-source API key is the credential, sent in
// Linear's bare (non-Bearer) form — the same shape the session-env push uses.
func TestLinearInject_APIKeyFallbackUsesBareForm(t *testing.T) {
	p := newTestProxy()
	p.SetLinearCredentialResolver(func() agent.LinearCredential { return agent.LinearCredential{APIKey: "lin_api_key"} })

	req := relayLinearRequest(t, p, agent.ModeIssuesOnly, "Bearer whatever")
	if got := req.Header.Get("Authorization"); got != "lin_api_key" {
		t.Fatalf("API-key fallback must be sent bare, got %q", got)
	}
}

// Below the ISSUES_ONLY floor nothing is injected: an ADVISORY agent's reads
// have always been forwarded as sent, and a write-capable credential must
// never be attached to a tier that is not allowed to write.
func TestLinearInject_NothingBelowIssuesOnlyFloor(t *testing.T) {
	p := newTestProxy()
	p.SetLinearCredentialResolver(func() agent.LinearCredential { return agent.LinearCredential{AccessToken: "lin_oauth_current"} })

	req := relayLinearRequest(t, p, agent.ModeAdvisory, "Bearer agents_own")
	if got := req.Header.Get("Authorization"); got != "Bearer agents_own" {
		t.Fatalf("ADVISORY request must be forwarded as sent, got %q", got)
	}
}

// No resolver (a GitHub-only hive) and an empty credential both leave the
// request exactly as the agent sent it.
func TestLinearInject_NoResolverOrEmptyCredentialLeavesRequestAlone(t *testing.T) {
	p := newTestProxy()
	req := relayLinearRequest(t, p, agent.ModeIssuesOnly, "Bearer agents_own")
	if got := req.Header.Get("Authorization"); got != "Bearer agents_own" {
		t.Fatalf("no resolver: got %q", got)
	}

	p.SetLinearCredentialResolver(func() agent.LinearCredential { return agent.LinearCredential{} })
	req = relayLinearRequest(t, p, agent.ModeIssuesOnly, "Bearer agents_own")
	if got := req.Header.Get("Authorization"); got != "Bearer agents_own" {
		t.Fatalf("empty credential: got %q", got)
	}
}

// The gate still runs first: a mutation an ADVISORY agent may not perform is
// refused before any credential could be attached — injection never widens
// the tier.
func TestLinearInject_DoesNotBypassTierGate(t *testing.T) {
	p := newTestProxy()
	injected := false
	p.SetLinearCredentialResolver(func() agent.LinearCredential {
		injected = true
		return agent.LinearCredential{AccessToken: "lin_oauth_current"}
	})

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()
	go p.proxyHTTPHost(proxyClient, proxyUpstream, "api.linear.app", "quality", agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := upstreamConn.Read(buf); err != nil {
				return
			}
		}
	}()
	body := `{"query":"mutation { issueCreate(input: {teamId: \"t\", title: \"x\"}) { issue { id } } }"}`
	fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.linear.app\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ADVISORY issueCreate must be refused, got %d", resp.StatusCode)
	}
	if injected {
		t.Fatal("the resolver must not be consulted for a request the gate refused")
	}
}

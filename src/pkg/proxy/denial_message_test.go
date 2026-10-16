package proxy

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// #12127: every proxy 403 is ONE line that ends in a machine-readable
// reason=<code>, and the same code rides the X-Hive-Proxy-Reason header.

func TestProxyDenialBody_OneLineWithReasonCode(t *testing.T) {
	body := ProxyDenialBody("scanner", agent.ModeAdvisory, "POST", "use hive-open-pr", DenyReasonHardDeny)
	if strings.Count(body, "\n") != 1 || !strings.HasSuffix(body, "\n") {
		t.Fatalf("denial body must be exactly one line, got %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "reason="+DenyReasonHardDeny) {
		t.Fatalf("denial body must end with reason=<code>, got %q", body)
	}
	for _, want := range []string{"scanner", "POST", "use hive-open-pr"} {
		if !strings.Contains(body, want) {
			t.Errorf("denial body lost %q (what/why/what-instead must stay): %q", want, body)
		}
	}
}

func TestProxyDenialBody_EmptyCodeFallsBackToGeneric(t *testing.T) {
	body := ProxyDenialBody("a", agent.ModeAdvisory, "GET", "/x", "")
	if !strings.Contains(body, "reason="+DenyReasonUnknown) {
		t.Fatalf("empty code must render the generic code, got %q", body)
	}
}

// A refused write carries the code for the gate that refused it — pause here,
// where the mode chain would have allowed the request — in both the body and
// the header, and the directive prose is intact beside it.
func TestProxyHTTP_DenialCarriesReasonCodeInBodyAndHeader(t *testing.T) {
	p := newTestProxy()
	p.SetRepoPausedFunc(pausedSet("org/repo"))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "quality", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "PATCH /repos/org/repo/issues/1 HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()

	raw := readAllWithDeadline(t, clientConn)
	if !strings.Contains(raw, "403") {
		t.Fatalf("want 403, got:\n%s", raw)
	}
	if !strings.Contains(raw, ProxyDenialHeader+": "+DenyReasonRepoPaused) {
		t.Errorf("403 lacks %s header with code %q:\n%s", ProxyDenialHeader, DenyReasonRepoPaused, raw)
	}
	bodyStart := strings.Index(raw, "\r\n\r\n")
	if bodyStart < 0 {
		t.Fatalf("no body in response:\n%s", raw)
	}
	body := strings.TrimRight(raw[bodyStart+4:], "\r\n")
	if strings.Contains(body, "\n") {
		t.Errorf("denial body is not one line:\n%s", body)
	}
	if !strings.HasSuffix(body, "reason="+DenyReasonRepoPaused) {
		t.Errorf("denial body must end with reason=%s:\n%s", DenyReasonRepoPaused, body)
	}
	if !strings.Contains(body, "PAUSED") || !strings.Contains(body, "org/repo") {
		t.Errorf("one-line denial dropped the actionable directive:\n%s", body)
	}
}

// A hard-deny (direct PR creation) keeps its "use hive-open-pr" directive and
// is tagged hard_deny, distinguishing it from a plain mode refusal.
func TestProxyHTTP_HardDenyReasonCode(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/pulls HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()

	raw := readAllWithDeadline(t, clientConn)
	if !strings.Contains(raw, "403") {
		t.Fatalf("want 403, got:\n%s", raw)
	}
	if !strings.Contains(raw, ProxyDenialHeader+": "+DenyReasonHardDeny) || !strings.Contains(raw, "reason="+DenyReasonHardDeny) {
		t.Errorf("hard deny must be coded %q in header and body:\n%s", DenyReasonHardDeny, raw)
	}
	if !strings.Contains(raw, "hive-open-pr") {
		t.Errorf("hard deny lost its directive:\n%s", raw)
	}
}

package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// pushSignoffLoopHarness drives proxyHTTPHost for github.com over in-memory
// pipes, the same path a MITM'd in-pod push takes (#19297). forwarded fires if
// any request reaches the upstream side; loopDone closes when the request
// loop returns.
type pushSignoffLoopHarness struct {
	client    net.Conn
	forwarded chan struct{}
	loopDone  chan struct{}
	cleanup   func()
}

func newPushSignoffLoopHarness(t *testing.T) *pushSignoffLoopHarness {
	t.Helper()
	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	h := &pushSignoffLoopHarness{
		client:    clientConn,
		forwarded: make(chan struct{}, 1),
		loopDone:  make(chan struct{}),
	}
	go func() {
		defer close(h.loopDone)
		p.proxyHTTPHost(proxyClient, proxyUpstream, "github.com", "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})
		_ = proxyClient.Close()
	}()
	go func() {
		if _, err := http.ReadRequest(bufio.NewReader(upstreamConn)); err == nil {
			h.forwarded <- struct{}{}
		}
	}()
	h.cleanup = func() {
		_ = clientConn.Close()
		_ = upstreamConn.Close()
		_ = proxyClient.Close()
		_ = proxyUpstream.Close()
	}
	t.Cleanup(h.cleanup)
	return h
}

// sendPush writes a receive-pack request carrying body, then next (if any) as
// a further request pipelined on the same connection.
func (h *pushSignoffLoopHarness) sendPush(body []byte, next string) {
	go func() {
		_, _ = fmt.Fprintf(h.client, "POST /o/r.git/git-receive-pack HTTP/1.1\r\nHost: github.com\r\nContent-Type: application/x-git-receive-pack-request\r\nContent-Length: %d\r\n\r\n", len(body))
		_, _ = h.client.Write(body)
		_, _ = io.WriteString(h.client, next)
	}()
}

func (h *pushSignoffLoopHarness) readResponse(t *testing.T) *http.Response {
	t.Helper()
	_ = h.client.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(h.client), nil)
	if err != nil {
		t.Fatalf("no response from the proxy loop: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp
}

// An unsigned lane commit pushed to github.com through the MITM loop is
// refused with the push_signoff reason and never reaches GitHub.
func TestProxyHTTPHostPushSignoffRefusesUnsignedLaneCommit(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	h := newPushSignoffLoopHarness(t)
	lane := "scanner <scanner@hive.kubestellar.io>"
	h.sendPush(testPackBody(t, [2]any{1, testCommit(lane, lane, "msg", 1)}, [2]any{3, "blob data"}), "")

	resp := h.readResponse(t)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get(ProxyDenialHeader); got != DenyReasonPushSignoff {
		t.Fatalf("%s = %q, want %q", ProxyDenialHeader, got, DenyReasonPushSignoff)
	}
	select {
	case <-h.forwarded:
		t.Fatal("refused push was forwarded upstream")
	case <-time.After(100 * time.Millisecond):
	}
}

// A push body over maxPushSignoffBody is refused, and because the rest of the
// upload is still on the wire the loop closes the connection instead of
// reading another request from it.
func TestProxyHTTPHostPushSignoffOversizeClosesConnection(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	h := newPushSignoffLoopHarness(t)
	// The push body is one byte over the cap. A well-formed request is
	// pipelined behind it, which a loop that kept reading the connection
	// would parse and forward upstream.
	cmd := pktCmd(" report-status")
	body := append(cmd, make([]byte, maxPushSignoffBody+1-len(cmd))...)
	h.sendPush(body, "GET /o/r.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: github.com\r\n\r\n")

	resp := h.readResponse(t)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get(ProxyDenialHeader); got != DenyReasonPushSignoff {
		t.Fatalf("%s = %q, want %q", ProxyDenialHeader, got, DenyReasonPushSignoff)
	}
	if !resp.Close {
		t.Fatal("oversize refusal must announce Connection: close")
	}
	// The client is still open with upload bytes pending; only an early
	// return from the loop ends it.
	select {
	case <-h.loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy loop kept reading the connection after an oversize push refusal")
	}
	select {
	case <-h.forwarded:
		t.Fatal("oversize push was forwarded upstream")
	default:
	}
}

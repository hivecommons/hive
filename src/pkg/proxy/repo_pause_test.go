package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// pausedSet turns a list of org-qualified repo names into the predicate the
// proxy takes, matching case-insensitively the way config's IsRepoPaused does.
func pausedSet(repos ...string) func(string) bool {
	set := make(map[string]bool, len(repos))
	for _, r := range repos {
		set[strings.ToLower(r)] = true
	}
	return func(repo string) bool { return set[strings.ToLower(repo)] }
}

// readAllWithDeadline drains whatever the proxy has already written. The
// blocked-response body carries no Content-Length (it is close-delimited), so a
// plain ReadAll would block until the connection closed; a deadline turns "no
// more bytes are coming" into a return instead of a hang.
func readAllWithDeadline(t *testing.T, c net.Conn) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, err := c.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
		// A refusal is one status line, a few headers and a single newline-
		// terminated line of body. Stop once that newline has arrived: stopping
		// at the header terminator would miss the body, and stopping at the
		// first byte after it can split a multi-byte character.
		if s := sb.String(); strings.Contains(s, "\r\n\r\n") && strings.Contains(s[strings.Index(s, "\r\n\r\n")+4:], "\n") {
			break
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	return sb.String()
}

// A paused repo refuses WRITES and only writes. Pause stops the hive acting on
// a repo, not looking at one — an agent that can still read can explain why it
// did nothing this session.
func TestRepoPauseRefusal_WritesOnly(t *testing.T) {
	paused := pausedSet("acme/frozen")

	writes := []struct{ method, path string }{
		{http.MethodPost, "/repos/acme/frozen/issues"},
		{http.MethodPatch, "/repos/acme/frozen/issues/7"},
		{http.MethodPost, "/repos/acme/frozen/issues/7/comments"},
		{http.MethodPost, "/repos/acme/frozen/pulls"},
		{http.MethodPut, "/repos/acme/frozen/pulls/7/merge"},
		{http.MethodDelete, "/repos/acme/frozen/git/refs/heads/x"},
		{http.MethodPost, "/acme/frozen.git/git-receive-pack"}, // git push
	}
	for _, tt := range writes {
		msg, refused := RepoPauseRefusal(paused, tt.method, tt.path)
		if !refused {
			t.Errorf("%s %s was not refused on a paused repo", tt.method, tt.path)
			continue
		}
		if !strings.Contains(msg, "acme/frozen") {
			t.Errorf("%s %s: refusal does not name the repo: %q", tt.method, tt.path, msg)
		}
	}

	reads := []struct{ method, path string }{
		{http.MethodGet, "/repos/acme/frozen/issues"},
		{http.MethodHead, "/repos/acme/frozen"},
		{http.MethodOptions, "/repos/acme/frozen/pulls"},
		{http.MethodPost, "/acme/frozen.git/git-upload-pack"}, // fetch: POST, but a read
	}
	for _, tt := range reads {
		if _, refused := RepoPauseRefusal(paused, tt.method, tt.path); refused {
			t.Errorf("%s %s was refused; pause must not block reads", tt.method, tt.path)
		}
	}
}

func TestRepoPauseRefusal_OnlyThePausedRepo(t *testing.T) {
	paused := pausedSet("acme/frozen")

	if _, refused := RepoPauseRefusal(paused, http.MethodPost, "/repos/acme/running/issues"); refused {
		t.Error("a write to an unpaused repo was refused")
	}
	// Same repo name, different org: a different repository.
	if _, refused := RepoPauseRefusal(paused, http.MethodPost, "/repos/other/frozen/issues"); refused {
		t.Error("a write to a same-named repo in another org was refused")
	}
	// Case folding: GitHub repo names are case-insensitive.
	if _, refused := RepoPauseRefusal(paused, http.MethodPost, "/repos/Acme/Frozen/issues"); !refused {
		t.Error("case-differing spelling of a paused repo was allowed")
	}
	// No predicate configured — every existing hive.
	if _, refused := RepoPauseRefusal(nil, http.MethodPost, "/repos/acme/frozen/issues"); refused {
		t.Error("refused a write with no pause predicate configured")
	}
	// A path carrying no repo cannot be attributed to one.
	if _, refused := RepoPauseRefusal(paused, http.MethodPost, "/graphql"); refused {
		t.Error("refused a path with no repo in it")
	}
}

// Pause is a run-state, not an autonomy tier: it must refuse a write from a
// MERGE-mode agent exactly as firmly as from an ADVISORY one. The mode chain
// would have allowed this exact request.
func TestProxyHTTP_PausedRepoRefusesMergeModeWrite(t *testing.T) {
	p := newTestProxy()
	p.SetRepoPausedFunc(pausedSet("org/repo"))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "quality", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	// Fail the test loudly if the request is forwarded instead of refused.
	forwarded := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		if n, _ := upstreamConn.Read(buf); n > 0 {
			forwarded <- struct{}{}
		}
	}()
	go func() {
		fmt.Fprintf(clientConn, "PATCH /repos/org/repo/issues/1 HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()

	raw := readAllWithDeadline(t, clientConn)
	if !strings.Contains(raw, "403") {
		t.Fatalf("want 403 for a write to a paused repo, got:\n%s", raw)
	}
	// The refusal must say the repo is PAUSED. A generic mode error would send
	// the agent hunting for a permissions bug that does not exist.
	if !strings.Contains(raw, "PAUSED") || !strings.Contains(raw, "org/repo") {
		t.Errorf("403 does not explain the pause:\n%s", raw)
	}
	if p.AgentViolations("quality") != 1 {
		t.Errorf("violations = %d, want 1 for a refused write", p.AgentViolations("quality"))
	}
	select {
	case <-forwarded:
		t.Error("the request reached upstream; a paused-repo write must never leave the proxy")
	default:
	}
}

// Resuming is not a restart: clearing the predicate must let the very next
// request through.
func TestProxyHTTP_ResumedRepoAllowsWriteAgain(t *testing.T) {
	p := newTestProxy()
	p.SetRepoPausedFunc(pausedSet("org/repo"))
	p.SetRepoPausedFunc(nil)

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesOnly, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()
	go respondOnce(upstreamConn, 201)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201 once the repo is resumed", resp.StatusCode)
	}
}

// A read against a paused repo still reaches GitHub.
func TestProxyHTTP_PausedRepoStillAllowsReads(t *testing.T) {
	p := newTestProxy()
	p.SetRepoPausedFunc(pausedSet("org/repo"))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	}()
	go respondOnce(upstreamConn, 200)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200: a paused repo stays readable", resp.StatusCode)
	}
}

// The hive's own control-plane traffic is exempt. Stopping it would take the
// whole spoke down to quiet one repo.
func TestRepoPause_InternalCallerExempt(t *testing.T) {
	p := newTestProxy()
	p.SetRepoPausedFunc(pausedSet("org/repo"))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, internalCallerName, agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()
	go respondOnce(upstreamConn, 201)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201: hive control-plane traffic is not agent activity", resp.StatusCode)
	}
}

// respondOnce reads one relayed request off the fake upstream and answers with
// an empty response of the given status.
func respondOnce(upstream net.Conn, status int) {
	req, err := http.ReadRequest(bufio.NewReader(upstream))
	if err != nil {
		return
	}
	resp := &http.Response{
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: make(http.Header), Body: http.NoBody, Request: req,
	}
	_ = resp.Write(upstream)
	_ = upstream.Close()
}

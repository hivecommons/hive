package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// scopeOf builds the predicate the proxy takes from a simple agent → repos map,
// matching case-insensitively the way config.AgentServesRepo does. An agent
// absent from the map is unscoped and serves everything.
func scopeOf(scopes map[string][]string) func(string, string) bool {
	return func(agentName, repo string) bool {
		want, ok := scopes[agentName]
		if !ok {
			return true
		}
		for _, r := range want {
			if strings.EqualFold(r, repo) {
				return true
			}
		}
		return false
	}
}

// readRefusal drains whatever the proxy has already written. The blocked
// response carries no Content-Length (it is close-delimited), so a plain
// ReadAll would block until the connection closed; a deadline plus a
// stop-at-the-body-newline rule turns "no more bytes are coming" into a return.
func readRefusal(t *testing.T, c net.Conn) string {
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
		if s := sb.String(); strings.Contains(s, "\r\n\r\n") && strings.Contains(s[strings.Index(s, "\r\n\r\n")+4:], "\n") {
			break
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	return sb.String()
}

func TestAgentRepoScopeRefusal_UsesReposFromAgentSpec(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "schema.yaml")
	if err := os.WriteFile(specPath, []byte(`name: schema
backend: copilot
model: auto
repos: [console]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`project:
  org: acme
  repos: [console, dashboard]
agents:
  schema:
    backend: copilot
    model: auto
    agent_spec: %q
github:
  token: test
`, specPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides: %v", err)
	}

	if _, refused := AgentRepoScopeRefusal(cfg.AgentServesRepo, "schema", http.MethodPost, "/repos/acme/console/issues"); refused {
		t.Fatal("refused an in-scope repo from the agent spec")
	}
	if _, refused := AgentRepoScopeRefusal(cfg.AgentServesRepo, "schema", http.MethodPost, "/repos/acme/dashboard/issues"); !refused {
		t.Fatal("allowed an out-of-scope repo because the spec repos were not wired into config")
	}
}

// The scope refuses writes and only writes. It says which repos an agent is
// FOR, not which repos it may look at: a reviewer scoped to the Go service may
// legitimately read the Rust CLI to understand a shared protocol.
func TestAgentRepoScopeRefusal_WritesOnly(t *testing.T) {
	serves := scopeOf(map[string][]string{"schema": {"acme/console"}})

	writes := []struct{ method, path string }{
		{http.MethodPost, "/repos/acme/dashboard/issues"},
		{http.MethodPatch, "/repos/acme/dashboard/issues/7"},
		{http.MethodPost, "/repos/acme/dashboard/issues/7/comments"},
		{http.MethodPost, "/repos/acme/dashboard/pulls"},
		{http.MethodPut, "/repos/acme/dashboard/pulls/7/merge"},
		{http.MethodDelete, "/repos/acme/dashboard/git/refs/heads/x"},
		{http.MethodPost, "/acme/dashboard.git/git-receive-pack"}, // push
	}
	for _, tt := range writes {
		msg, refused := AgentRepoScopeRefusal(serves, "schema", tt.method, tt.path)
		if !refused {
			t.Errorf("%s %s was not refused for an out-of-scope agent", tt.method, tt.path)
			continue
		}
		if !strings.Contains(msg, "schema") || !strings.Contains(msg, "acme/dashboard") {
			t.Errorf("%s %s: refusal names neither the agent nor the repo: %q", tt.method, tt.path, msg)
		}
	}

	reads := []struct{ method, path string }{
		{http.MethodGet, "/repos/acme/dashboard/issues"},
		{http.MethodHead, "/repos/acme/dashboard"},
		{http.MethodOptions, "/repos/acme/dashboard/pulls"},
		{http.MethodPost, "/acme/dashboard.git/git-upload-pack"}, // fetch: POST, but a read
	}
	for _, tt := range reads {
		if _, refused := AgentRepoScopeRefusal(serves, "schema", tt.method, tt.path); refused {
			t.Errorf("%s %s was refused; scope must not block reads", tt.method, tt.path)
		}
	}
}

func TestAgentRepoScopeRefusal_FailsOpen(t *testing.T) {
	serves := scopeOf(map[string][]string{"schema": {"acme/console"}})

	// In-scope write.
	if _, refused := AgentRepoScopeRefusal(serves, "schema", http.MethodPost, "/repos/acme/console/issues"); refused {
		t.Error("refused a write to the agent's own repo")
	}
	// An agent with no scope serves everything.
	if _, refused := AgentRepoScopeRefusal(serves, "scanner", http.MethodPost, "/repos/acme/dashboard/issues"); refused {
		t.Error("refused an unscoped agent")
	}
	// No predicate configured — every hive before this feature.
	if _, refused := AgentRepoScopeRefusal(nil, "schema", http.MethodPost, "/repos/acme/dashboard/issues"); refused {
		t.Error("refused a write with no scope predicate configured")
	}
	// An unidentified caller cannot be scope-checked; the mode chain still
	// blocks its writes, so failing open here does not open a hole.
	if _, refused := AgentRepoScopeRefusal(serves, "", http.MethodPost, "/repos/acme/dashboard/issues"); refused {
		t.Error("refused a write from an unnamed agent")
	}
	// A path carrying no repo cannot be attributed to one.
	if _, refused := AgentRepoScopeRefusal(serves, "schema", http.MethodPost, "/graphql"); refused {
		t.Error("refused a path with no repo in it")
	}
}

// Scope is roster membership, not autonomy: it must refuse a MERGE-mode agent
// exactly as firmly as an ADVISORY one. The mode chain would have allowed this
// exact request.
func TestProxyHTTP_OutOfScopeAgentRefusedAtMergeMode(t *testing.T) {
	p := newTestProxy()
	p.SetAgentRepoScopeFunc(scopeOf(map[string][]string{"schema": {"org/other"}}))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "schema", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
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

	raw := readRefusal(t, clientConn)
	if !strings.Contains(raw, "403") {
		t.Fatalf("want 403 for an out-of-scope write, got:\n%s", raw)
	}
	if !strings.Contains(raw, "NOT scoped") || !strings.Contains(raw, "org/repo") {
		t.Errorf("403 does not explain the scope:\n%s", raw)
	}
	if p.AgentViolations("schema") != 1 {
		t.Errorf("violations = %d, want 1", p.AgentViolations("schema"))
	}
	select {
	case <-forwarded:
		t.Error("the request reached upstream; an out-of-scope write must never leave the proxy")
	default:
	}
}

func TestProxyHTTP_InScopeAgentWritesNormally(t *testing.T) {
	p := newTestProxy()
	p.SetAgentRepoScopeFunc(scopeOf(map[string][]string{"schema": {"org/repo"}}))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "schema", agent.ModeIssuesOnly, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()
	go answerOnce(upstreamConn, 201)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201 for a write to the agent's own repo", resp.StatusCode)
	}
}

// An out-of-scope repo stays readable: an agent that can still read can orient
// itself and explain why it did nothing.
func TestProxyHTTP_OutOfScopeReadStillRelayed(t *testing.T) {
	p := newTestProxy()
	p.SetAgentRepoScopeFunc(scopeOf(map[string][]string{"schema": {"org/other"}}))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "schema", agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	}()
	go answerOnce(upstreamConn, 200)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200: an out-of-scope repo stays readable", resp.StatusCode)
	}
}

// The hive's own control-plane traffic is exempt: scope composes the AGENT
// roster, and refusing the hive itself would take the spoke down.
func TestProxyHTTP_InternalCallerExemptFromScope(t *testing.T) {
	p := newTestProxy()
	p.SetAgentRepoScopeFunc(func(string, string) bool { return false })

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, internalCallerName, agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()
	go answerOnce(upstreamConn, 201)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201: hive control-plane traffic is not agent activity", resp.StatusCode)
	}
}

// answerOnce reads one relayed request off the fake upstream and replies with
// an empty response of the given status.
func answerOnce(upstream net.Conn, status int) {
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

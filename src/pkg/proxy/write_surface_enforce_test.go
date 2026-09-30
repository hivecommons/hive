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

// enforceOnly is the shape config.WriteSurfaceEnforced has for a hive that
// lists the given lanes under write_surface.enforce.
func enforceOnly(lanes ...string) func(string) bool {
	return func(agentName string) bool {
		for _, l := range lanes {
			if l == agentName {
				return true
			}
		}
		return false
	}
}

type refusedWrite struct {
	agent, kind, method, path, repo string
}

func captureWriteRefusals(p *GitHubProxy) <-chan refusedWrite {
	ch := make(chan refusedWrite, 4)
	p.SetWriteRefusedAuditFunc(func(agentName, kind, method, path, repo string) {
		ch <- refusedWrite{agentName, kind, method, path, repo}
	})
	return ch
}

func TestWriteSurfaceEnforceRefusal_Classification(t *testing.T) {
	enforced := enforceOnly("scanner")
	cases := []struct {
		name, agent, method, path, query string
		wantKind                         string
	}{
		{"rest post", "scanner", "POST", "/repos/o/r/issues", "", WriteSurfaceKindREST},
		{"rest patch", "scanner", "PATCH", "/repos/o/r/pulls/1", "", WriteSurfaceKindREST},
		{"rest put merge", "scanner", "PUT", "/repos/o/r/pulls/1/merge", "", WriteSurfaceKindREST},
		{"rest delete", "scanner", "DELETE", "/repos/o/r/git/refs/heads/x", "", WriteSurfaceKindREST},
		{"git push", "scanner", "POST", "/o/r.git/git-receive-pack", "", WriteSurfaceKindGitPush},
		{"git push no .git", "scanner", "POST", "/o/r/git-receive-pack", "", WriteSurfaceKindGitPush},
		{"push ref advertisement", "scanner", "GET", "/o/r.git/info/refs", "service=git-receive-pack", WriteSurfaceKindGitPush},
		{"fetch ref advertisement", "scanner", "GET", "/o/r.git/info/refs", "service=git-upload-pack", ""},
		{"git fetch", "scanner", "POST", "/o/r.git/git-upload-pack", "", ""},
		{"rest read", "scanner", "GET", "/repos/o/r/issues", "", ""},
		{"graphql left to body check", "scanner", "POST", "/graphql", "", ""},
		{"device login", "scanner", "POST", "/login/device/code", "", ""},
		{"oauth token", "scanner", "POST", "/login/oauth/access_token", "", ""},
		{"unlisted lane", "reviewer", "POST", "/repos/o/r/issues", "", ""},
		{"unlisted lane push", "reviewer", "POST", "/o/r.git/git-receive-pack", "", ""},
		{"unnamed agent", "", "POST", "/repos/o/r/issues", "", ""},
	}
	for _, tc := range cases {
		reason, kind, refused := WriteSurfaceEnforceRefusal(enforced, tc.agent, tc.method, tc.path, tc.query)
		if refused != (tc.wantKind != "") || kind != tc.wantKind {
			t.Errorf("%s: refused=%v kind=%q, want kind %q", tc.name, refused, kind, tc.wantKind)
		}
		if refused && (!strings.Contains(reason, "write_surface.enforce") || !strings.Contains(reason, "hive-")) {
			t.Errorf("%s: reason does not name the setting and the relay to use: %q", tc.name, reason)
		}
	}
}

func TestWriteSurfaceEnforceRefusal_DefaultOff(t *testing.T) {
	if _, _, refused := WriteSurfaceEnforceRefusal(nil, "scanner", "POST", "/repos/o/r/issues", ""); refused {
		t.Error("nil predicate refused a write; enforcement must be opt-in")
	}
	if _, refused := WriteSurfaceGraphQLRefusal(nil, "scanner", []byte(`{"query":"mutation { x }"}`)); refused {
		t.Error("nil predicate refused a GraphQL mutation; enforcement must be opt-in")
	}
}

func TestWriteSurfaceGraphQLRefusal(t *testing.T) {
	enforced := enforceOnly("scanner")
	cases := []struct {
		name, agent, body string
		want              bool
	}{
		{"mutation", "scanner", `{"query":"mutation { addComment(input:{}) { clientMutationId } }"}`, true},
		{"query", "scanner", `{"query":"query { viewer { login } }"}`, false},
		{"shorthand query", "scanner", `{"query":"{ viewer { login } }"}`, false},
		{"not json fails closed", "scanner", `not json`, true},
		{"unlisted lane mutation", "reviewer", `{"query":"mutation { x }"}`, false},
	}
	for _, tc := range cases {
		if _, got := WriteSurfaceGraphQLRefusal(enforced, tc.agent, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: refused=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWriteSurfaceRepo(t *testing.T) {
	cases := map[string]string{
		"/repos/o/r/issues":         "o/r",
		"/o/r.git/git-receive-pack": "o/r",
		"/o/r/git-receive-pack":     "o/r",
		"/o/r/info/refs":            "o/r",
		"/graphql":                  "",
		"/user/repos":               "",
	}
	for path, want := range cases {
		if got := WriteSurfaceRepo(path); got != want {
			t.Errorf("WriteSurfaceRepo(%q) = %q, want %q", path, got, want)
		}
	}
}

// An enforced lane has github.com intercepted too, so git push can be refused;
// an unlisted lane keeps the historical opaque tunnel.
func TestHostNeedsMITMFor_EnforcedLaneInterceptsGitHost(t *testing.T) {
	p := newTestProxy()
	if p.hostNeedsMITMFor("github.com", "scanner") {
		t.Fatal("github.com intercepted with enforcement unset; default must be unchanged")
	}
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))
	if !p.hostNeedsMITMFor("github.com", "scanner") {
		t.Error("enforced lane: github.com must be intercepted so git push can be refused")
	}
	if p.hostNeedsMITMFor("github.com", "reviewer") {
		t.Error("unlisted lane: github.com must stay an opaque tunnel")
	}
	if p.hostNeedsMITMFor("github.com", internalCallerName) {
		t.Error("the hive's own control plane is never enforced")
	}
	if p.hostNeedsMITMFor("example.com", "scanner") {
		t.Error("a non-GitHub host must never be intercepted for enforcement")
	}
	if !p.hostNeedsMITMFor("api.github.com", "reviewer") {
		t.Error("api.github.com is always intercepted")
	}
}

// An enforced lane is refused even at MERGE mode — which the mode chain would
// have allowed — and the refusal is audited and never reaches upstream.
func TestProxyHTTP_EnforcedLaneDirectWriteRefusedAndAudited(t *testing.T) {
	p := newTestProxy()
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))
	audits := captureWriteRefusals(p)

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	forwarded := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 4096)
		if n, _ := upstreamConn.Read(buf); n > 0 {
			forwarded <- struct{}{}
		}
	}()
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues/1/comments HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()

	raw := readRefusal(t, clientConn)
	if !strings.Contains(raw, "403") || !strings.Contains(raw, "write_surface.enforce") {
		t.Fatalf("want a 403 naming write_surface.enforce, got:\n%s", raw)
	}
	select {
	case got := <-audits:
		want := refusedWrite{"scanner", WriteSurfaceKindREST, "POST", "/repos/org/repo/issues/1/comments", "org/repo"}
		if got != want {
			t.Errorf("audit = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refusal was not audited")
	}
	if p.AgentViolations("scanner") != 1 {
		t.Errorf("violations = %d, want 1", p.AgentViolations("scanner"))
	}
	select {
	case <-forwarded:
		t.Error("the request reached upstream; an enforced lane's direct write must never leave the proxy")
	default:
	}
}

func TestProxyHTTP_EnforcedLaneGitPushRefused(t *testing.T) {
	p := newTestProxy()
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))
	audits := captureWriteRefusals(p)

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTPHost(proxyClient, proxyUpstream, "github.com", "scanner", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "GET /org/repo.git/info/refs?service=git-receive-pack HTTP/1.1\r\nHost: github.com\r\n\r\n")
	}()

	raw := readRefusal(t, clientConn)
	if !strings.Contains(raw, "403") || !strings.Contains(raw, "hive-push-branch") {
		t.Fatalf("want a 403 pointing at hive-push-branch, got:\n%s", raw)
	}
	select {
	case got := <-audits:
		if got.kind != WriteSurfaceKindGitPush || got.repo != "org/repo" {
			t.Errorf("audit = %+v, want kind %q repo org/repo", got, WriteSurfaceKindGitPush)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refused push was not audited")
	}
}

func TestProxyHTTP_EnforcedLaneGraphQLMutationRefused(t *testing.T) {
	p := newTestProxy()
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))
	audits := captureWriteRefusals(p)

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	body := `{"query":"mutation { addComment(input:{subjectId:\"x\",body:\"y\"}) { clientMutationId } }"}`
	go func() {
		fmt.Fprintf(clientConn, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()

	raw := readRefusal(t, clientConn)
	if !strings.Contains(raw, "403") || !strings.Contains(raw, "write_surface.enforce") {
		t.Fatalf("want a 403 naming write_surface.enforce, got:\n%s", raw)
	}
	select {
	case got := <-audits:
		if got.kind != WriteSurfaceKindGraphQL {
			t.Errorf("audit kind = %q, want %q", got.kind, WriteSurfaceKindGraphQL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refused mutation was not audited")
	}
}

// Reads stay broad for an enforced lane.
func TestProxyHTTP_EnforcedLaneReadStillRelayed(t *testing.T) {
	p := newTestProxy()
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeAdvisory, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "GET /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\n\r\n")
	}()
	go answerOnce(upstreamConn, 200)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200: an enforced lane can still read", resp.StatusCode)
	}
}

// Enforcement is per lane: a lane that is not listed keeps its direct writes.
func TestProxyHTTP_UnlistedLaneWritesDirectly(t *testing.T) {
	p := newTestProxy()
	p.SetWriteSurfaceEnforceFunc(enforceOnly("scanner"))
	audits := captureWriteRefusals(p)

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "reviewer", agent.ModeIssuesOnly, agent.AgentCapabilities{})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues HTTP/1.1\r\nHost: api.github.com\r\nContent-Length: 0\r\n\r\n")
	}()
	go answerOnce(upstreamConn, 201)

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201: an unlisted lane keeps direct access", resp.StatusCode)
	}
	select {
	case got := <-audits:
		t.Errorf("unlisted lane's write was audited as refused: %+v", got)
	default:
	}
}

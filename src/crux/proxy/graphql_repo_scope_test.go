package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
)

// Each exchange uses a fresh connection, as gh does between lookup and write.
// Wait for BOTH fake upstream and proxy to exit, including on assertion failure.
func graphQLScopeExchange(t *testing.T, p *GitHubProxy, request, reply []byte) (status int, headers http.Header, response string, reached bool) {
	t.Helper()
	// Go's HTTP transport (and gh) normally negotiates gzip. Keep that wire
	// shape in the integration tests, not just an uncompressed mock response.
	reply = gzipScopeResponse(t, reply)
	client, proxyClient := net.Pipe()
	upstream, proxyUpstream := net.Pipe()
	for _, conn := range []net.Conn{client, proxyClient, upstream, proxyUpstream} {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		defer proxyClient.Close()
		defer proxyUpstream.Close()
		p.proxyHTTP(proxyClient, proxyUpstream, "schema", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})
	}()
	forwarded := make(chan bool, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstream))
		if err != nil {
			forwarded <- false
			return
		}
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil || !bytes.Equal(body, request) {
			forwarded <- false
			return
		}
		_, _ = fmt.Fprintf(upstream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s", len(reply), reply)
		forwarded <- true
	}()
	defer func() {
		_ = client.Close()
		_ = upstream.Close()
		<-proxyDone
		reached = <-forwarded
	}()
	_, err := fmt.Fprintf(client, "POST /graphql HTTP/1.1\r\nHost: api.github.com\r\nAccept-Encoding: gzip\r\nContent-Length: %d\r\n\r\n%s", len(request), request)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	var responseReader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		decoded, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer decoded.Close()
		responseReader = decoded
	}
	var body []byte
	if resp.StatusCode == http.StatusForbidden {
		// Proxy denials are a single line with no Content-Length. The
		// keep-alive proxy waits for another request, so waiting for EOF
		// here deadlocks until the pipe deadline instead of reading a denial.
		line, readErr := bufio.NewReader(responseReader).ReadString('\n')
		body, err = []byte(line), readErr
		_ = client.Close()
	} else {
		body, err = io.ReadAll(responseReader)
	}
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Deferred teardown waits for response observation and sets reached
	// before the caller can begin the next exchange.
	return resp.StatusCode, resp.Header, string(body), false
}

func scopeGraphQLBody(query string, variables any) []byte {
	b, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	return b
}

func TestGraphQLRepoScopeGHFlows(t *testing.T) {
	// Scope is independent of autonomy. Other package tests register global
	// overrides, which demote unattributed PR creates/merges to advisory.
	// Snapshot and clear them so these flows exercise only repository scope;
	// restore them afterwards, and do not run this test in parallel.
	saved := map[any]any{}
	autonomyRepoLevels.Range(func(k, v any) bool {
		saved[k] = v
		autonomyRepoLevels.Delete(k)
		return true
	})
	t.Cleanup(func() {
		for k, v := range saved {
			autonomyRepoLevels.Store(k, v)
		}
	})
	data, err := os.ReadFile("testdata/graphql-gh-repo-scope.json")
	if err != nil {
		t.Fatal(err)
	}
	var flows []struct {
		Command                 string
		Lookup, Response, Write json.RawMessage
	}
	if err := json.Unmarshal(data, &flows); err != nil {
		t.Fatal(err)
	}
	for _, flow := range flows {
		for _, inScope := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/inScope=%v", flow.Command, inScope), func(t *testing.T) {
				p := newTestProxy()
				p.SetAgentRepoScopeFunc(func(_, repo string) bool { return repo == "acme/widgets" && inScope })
				// Cross-repo reads stay unrestricted, even when no repo is served.
				status, _, body, reached := graphQLScopeExchange(t, p, flow.Lookup, flow.Response)
				if status != 200 || !reached || body != string(flow.Response) {
					t.Fatalf("lookup = %d, reached=%v, body=%s", status, reached, body)
				}
				status, headers, body, reached := graphQLScopeExchange(t, p, flow.Write, []byte(`{"data":{}}`))
				if inScope {
					if status != 200 || !reached {
						t.Fatalf("in-scope write = %d, reached=%v: %s", status, reached, body)
					}
				} else {
					if status != 403 || reached {
						t.Fatalf("out-of-scope write = %d, reached=%v: %s", status, reached, body)
					}
					if headers.Get(ProxyDenialHeader) != DenyReasonAgentRepoScope || headers.Get("X-Hive-Proxy-Blocked") != "true" || !strings.Contains(body, "NOT scoped to acme/widgets") {
						t.Fatalf("not a REST-shaped scope denial: headers=%v body=%s", headers, body)
					}
					reason, _ := p.agentRepoScopeRefusal("schema", "POST", "/repos/acme/widgets")
					wantBody := ProxyDenialBody("schema", agent.ModeIssuesPRsMerge, "POST", reason, DenyReasonAgentRepoScope)
					if body != wantBody || headers.Get("Content-Type") != "text/plain" {
						t.Fatalf("denial differs from REST: %s", body)
					}
					if p.AgentViolations("schema") != 1 {
						t.Fatal("scope denial was not audited")
					}
				}
			})
		}
		t.Run(flow.Command+"/cold-cache", func(t *testing.T) {
			p := newTestProxy()
			p.SetAgentRepoScopeFunc(func(string, string) bool { return false })
			status, _, body, reached := graphQLScopeExchange(t, p, flow.Write, []byte(`{"data":{}}`))
			if status != 200 || !reached {
				t.Fatalf("unresolved write broke existing behavior: %d %s", status, body)
			}
		})
	}
}

func TestGraphQLRepoScopeArgumentsAndOperations(t *testing.T) {
	p := newTestProxy()
	p.SetAgentRepoScopeFunc(func(_, repo string) bool { return repo == "acme/open" })
	p.graphQLRepos.put("schema", "I_closed", "acme/closed")
	for _, tc := range []struct {
		name, query string
		vars        any
		refused     bool
	}{
		{"inline input", `mutation { createCommitOnBranch(input:{branch:{repositoryNameWithOwner:"acme/closed",branchName:"main"},message:{headline:"hi"},expectedHeadOid:"abc"}){clientMutationId} }`, nil, true},
		{"inline cached node", `mutation { addComment(input:{subjectId:"I_closed",body:"hi"}){clientMutationId} }`, nil, true},
		{"variable input", `mutation($input:CreateCommitOnBranchInput!){createCommitOnBranch(input:$input){clientMutationId}}`, map[string]any{"input": map[string]any{"branch": map[string]any{"repositoryNameWithOwner": "acme/closed"}}}, true},
		{"repository selection", `mutation { repository(name:"closed",owner:"acme"){id} }`, nil, true},
		{"in scope", `mutation { repository(owner:"acme",name:"open"){id} }`, nil, false},
		{"all targets", `mutation { a:addComment(input:{subjectId:"I_unknown",body:"hi"}){clientMutationId} b:addComment(input:{subjectId:"I_closed",body:"hi"}){clientMutationId} }`, nil, true},
		{"fragment", `mutation { ...Write } fragment Write on Mutation { addComment(input:{subjectId:"I_closed",body:"hi"}){clientMutationId} }`, nil, true},
		{"default variable", `mutation($id:ID!="I_closed"){addComment(input:{subjectId:$id,body:"hi"}){clientMutationId}}`, nil, true},
		{"read", `query { repository(owner:"acme",name:"closed"){id} }`, nil, false},
		{"body is not a target", `mutation { addComment(input:{subjectId:"I_unknown",body:"acme/closed"}){clientMutationId} }`, nil, false},
		{"unused variables", `mutation { addComment(input:{subjectId:"I_unknown",body:"hi"}){clientMutationId} }`, map[string]any{"repo": "acme/closed"}, false},
		{"unresolved", `mutation { addComment(input:{subjectId:"I_unknown",body:"hi"}){clientMutationId} }`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refused := p.graphQLAgentRepoScopeRefusal("schema", scopeGraphQLBody(tc.query, tc.vars))
			if refused != tc.refused {
				t.Fatalf("refused=%v, want %v", refused, tc.refused)
			}
		})
	}
	for _, selected := range []string{"Read", "Write"} {
		body, _ := json.Marshal(map[string]any{"query": `query Read { repository(owner:"acme",name:"closed"){id} } mutation Write { addComment(input:{subjectId:"I_closed",body:"hi"}){clientMutationId} }`, "operationName": selected})
		_, refused := p.graphQLAgentRepoScopeRefusal("schema", body)
		if refused != (selected == "Write") {
			t.Fatalf("operationName=%s refused=%v", selected, refused)
		}
	}
	if _, refused := p.graphQLAgentRepoScopeRefusal("schema", []byte(`not json`)); refused {
		t.Fatal("unparseable request gained a new scope denial")
	}
}

func TestGraphQLRepoScopeCacheAttribution(t *testing.T) {
	p := newTestProxy()
	lookup := scopeGraphQLBody(`query($owner:String!,$name:String!){ r:repository(owner:$owner,name:$name){ key:id issue(number:1){ key:id } } }`, map[string]any{"owner": "acme", "name": "widgets"})
	p.observeGraphQLRepos("schema", lookup, []byte(`{"data":{"r":{"key":"R1","issue":{"key":"I1"}}}}`))
	if p.graphQLRepos.get("schema", "R1") != "acme/widgets" || p.graphQLRepos.get("schema", "I1") != "acme/widgets" {
		t.Fatal("aliased lookup did not resolve IDs")
	}
	if p.graphQLRepos.get("other", "I1") != "" {
		t.Fatal("cache leaked across agents")
	}

	lookup = scopeGraphQLBody(`query { nodes(ids:["I2"]){ ... on Issue { id repository { nameWithOwner } } } }`, nil)
	p.observeGraphQLRepos("schema", lookup, []byte(`{"data":{"nodes":[{"id":"I2","repository":{"nameWithOwner":"acme/other"}}]}}`))
	if p.graphQLRepos.get("schema", "I2") != "acme/other" {
		t.Fatal("node's repository did not resolve")
	}
	// A caller can alias free-form body text to a trusted-looking key, but
	// cannot turn it into an id or a repo in the cache.
	lookup = scopeGraphQLBody(`query { repository(owner:"acme",name:"widgets"){issue(number:1){id:body}} }`, nil)
	p.observeGraphQLRepos("schema", lookup, []byte(`{"data":{"repository":{"issue":{"id":"forged"}}}}`))
	if p.graphQLRepos.get("schema", "forged") != "" {
		t.Fatal("body alias poisoned ID cache")
	}
	// A read with caller-chosen repo metadata must not rebind an existing ID.
	p.graphQLAgentRepoScopeRefusal("schema", scopeGraphQLBody(`query { node(id:"I2"){id} }`, map[string]any{"repo": "acme/widgets"}))
	if p.graphQLRepos.get("schema", "I2") != "acme/other" {
		t.Fatal("request poisoned cache")
	}
	p.observeGraphQLRepos("schema", lookup, []byte(`not json`))
}

func TestGraphQLRepoScopeLivePolicyAndCacheBounds(t *testing.T) {
	p := newTestProxy()
	p.graphQLRepos.put("schema", "I", "acme/widgets")
	body := scopeGraphQLBody(`mutation { closeIssue(input:{issueId:"I"}){clientMutationId} }`, nil)
	for _, allowed := range []bool{true, false, true} {
		p.SetAgentRepoScopeFunc(func(string, string) bool { return allowed })
		if _, refused := p.graphQLAgentRepoScopeRefusal("schema", body); refused == allowed {
			t.Fatalf("scope update allowed=%v refused=%v", allowed, refused)
		}
	}
	for i := 0; i < graphQLRepoCacheLimit+5; i++ {
		p.graphQLRepos.put("schema", fmt.Sprint(i), "acme/widgets")
	}
	if len(p.graphQLRepos.repos) > graphQLRepoCacheLimit {
		t.Fatal("unbounded cache")
	}
	capture := &graphQLRepoCapture{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", graphQLRepoResponseLimit+1)))}
	if _, err := io.Copy(io.Discard, capture); err != nil {
		t.Fatal(err)
	}
	if !capture.tooLarge || capture.buf.Len() != 0 {
		t.Fatal("oversized response was retained")
	}
}

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/prosefilter"
)

const (
	em = prosefilter.EmDash
	en = prosefilter.EnDash
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decodeField returns the string at a dotted path of a JSON document.
func decodeField(t *testing.T, body []byte, path ...string) string {
	t.Helper()
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, body)
	}
	for _, p := range path {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			i, _ := strconv.Atoi(p)
			cur = c[i]
		}
	}
	s, _ := cur.(string)
	return s
}

func TestSanitizeAgentWriteProse(t *testing.T) {
	fence := "```\nfile " + em + " list\n```"
	cases := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		linear      bool
		agent       string
		off         bool
		// field is the JSON path checked; want is its value after the filter.
		field []string
		want  string
		// unchanged means the forwarded body must equal the input byte-for-byte.
		unchanged bool
	}{
		{name: "REST issue comment", method: "POST", path: "/repos/o/r/issues/1/comments",
			body: mustJSON(t, map[string]any{"body": "looks good " + em + " merging"}), field: []string{"body"}, want: "looks good - merging"},
		{name: "PR body edit", method: "PATCH", path: "/repos/o/r/pulls/2",
			body: mustJSON(t, map[string]any{"body": "Fixes x " + em + " and y", "state": "open"}), field: []string{"body"}, want: "Fixes x - and y"},
		{name: "review thread reply", method: "POST", path: "/repos/o/r/pulls/2/comments/9/replies",
			body: mustJSON(t, map[string]any{"body": "done " + em + " see abc123"}), field: []string{"body"}, want: "done - see abc123"},
		{name: "PR title", method: "POST", path: "/repos/o/r/pulls",
			body: mustJSON(t, map[string]any{"title": "fix: a " + em + " b", "head": "x" + em + "y", "base": "main"}), field: []string{"title"}, want: "fix: a - b"},
		{name: "review with inline comments", method: "POST", path: "/repos/o/r/pulls/2/reviews",
			body:  mustJSON(t, map[string]any{"event": "COMMENT", "comments": []any{map[string]any{"path": "a.go", "line": 3, "body": "nit " + em + " rename"}}}),
			field: []string{"comments", "0", "body"}, want: "nit - rename"},
		{name: "contents API commit message", method: "PUT", path: "/repos/o/r/contents/a.md",
			body: mustJSON(t, map[string]any{"message": "docs: a " + em + " b", "content": "YQ=="}), field: []string{"message"}, want: "docs: a - b"},
		{name: "fenced code block kept", method: "POST", path: "/repos/o/r/issues/1/comments",
			body: mustJSON(t, map[string]any{"body": "x " + em + " y\n" + fence}), field: []string{"body"}, want: "x - y\n" + fence},
		{name: "en-dash range kept", method: "POST", path: "/repos/o/r/issues/1/comments",
			body: mustJSON(t, map[string]any{"body": "levels 3" + en + "5"}), unchanged: true},
		{name: "non-text field untouched", method: "POST", path: "/repos/o/r/git/refs",
			body: mustJSON(t, map[string]any{"ref": "refs/heads/a" + em + "b", "sha": "abc"}), unchanged: true},
		{name: "non-JSON content type untouched", method: "POST", path: "/repos/o/r/releases/1/assets",
			contentType: "application/octet-stream", body: "raw " + em + " bytes", unchanged: true},
		{name: "body that is not JSON untouched", method: "POST", path: "/repos/o/r/issues/1/comments",
			body: "plain " + em + " text", unchanged: true},
		{name: "git push untouched", method: "POST", path: "/o/r.git/git-receive-pack",
			contentType: "application/x-git-receive-pack-request", body: "0000 " + em, unchanged: true},
		{name: "LFS batch untouched", method: "POST", path: "/o/r.git/info/lfs/objects/batch",
			body: mustJSON(t, map[string]any{"message": "a " + em + " b"}), unchanged: true},
		{name: "GET untouched", method: "GET", path: "/repos/o/r/issues",
			body: mustJSON(t, map[string]any{"body": "a " + em + " b"}), unchanged: true},
		{name: "switch off", method: "POST", path: "/repos/o/r/issues/1/comments", off: true,
			body: mustJSON(t, map[string]any{"body": "a " + em + " b"}), unchanged: true},
		{name: "hive control plane untouched", method: "POST", path: "/repos/o/r/issues/1/comments", agent: internalCallerName,
			body: mustJSON(t, map[string]any{"body": "a " + em + " b"}), unchanged: true},
		{name: "GraphQL review thread reply", method: "POST", path: "/graphql",
			body: mustJSON(t, map[string]any{
				"query":     "mutation($input: AddPullRequestReviewThreadReplyInput!) { addPullRequestReviewThreadReply(input: $input) { comment { id } } }",
				"variables": map[string]any{"input": map[string]any{"pullRequestReviewThreadId": "T_1", "body": "fixed " + em + " thanks"}},
			}), field: []string{"variables", "input", "body"}, want: "fixed - thanks"},
		{name: "GraphQL createPullRequest title", method: "POST", path: "/graphql",
			body: mustJSON(t, map[string]any{
				"query":     "mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { url } } }",
				"variables": map[string]any{"input": map[string]any{"title": "feat: x " + em + " y", "body": "b", "baseRefName": "main", "headRefName": "f"}},
			}), field: []string{"variables", "input", "title"}, want: "feat: x - y"},
		{name: "GraphQL query untouched", method: "POST", path: "/graphql",
			body: mustJSON(t, map[string]any{"query": "query { viewer { login } }", "variables": map[string]any{"body": "a " + em + " b"}}), unchanged: true},
		{name: "GraphQL unrelated mutation untouched", method: "POST", path: "/graphql",
			body: mustJSON(t, map[string]any{"query": "mutation($id: ID!) { addLabelsToLabelable(input: {labelableId: $id}) { clientMutationId } }", "variables": map[string]any{"title": "a " + em + " b"}}), unchanged: true},
		{name: "Linear issue description", method: "POST", path: "/graphql", linear: true,
			body: mustJSON(t, map[string]any{
				"query":     "mutation($input: IssueCreateInput!) { issueCreate(input: $input) { success } }",
				"variables": map[string]any{"input": map[string]any{"title": "t", "description": "a " + em + " b"}},
			}), field: []string{"variables", "input", "description"}, want: "a - b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProxy()
			p.proseFilterOff = tc.off
			agentName := tc.agent
			if agentName == "" {
				agentName = "quality"
			}
			req, err := http.NewRequest(tc.method, "https://api.github.com"+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if err := p.sanitizeAgentWriteProse(req, agentName, tc.linear); err != nil {
				t.Fatalf("sanitizeAgentWriteProse: %v", err)
			}
			got, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.unchanged {
				if string(got) != tc.body {
					t.Fatalf("body changed:\n got %s\nwant %s", got, tc.body)
				}
				return
			}
			if req.ContentLength != int64(len(got)) {
				t.Errorf("ContentLength = %d, body is %d bytes", req.ContentLength, len(got))
			}
			if h := req.Header.Get("Content-Length"); h != strconv.Itoa(len(got)) {
				t.Errorf("Content-Length header = %q, body is %d bytes", h, len(got))
			}
			if v := decodeField(t, got, tc.field...); v != tc.want {
				t.Errorf("%v = %q, want %q", tc.field, v, tc.want)
			}
		})
	}
}

// A body bigger than the buffer bound is forwarded whole and unchanged.
func TestSanitizeAgentWriteProseOversizedBodyForwardedIntact(t *testing.T) {
	p := newTestProxy()
	big := mustJSON(t, map[string]any{"body": em + strings.Repeat("x", outboundProseBodyLimit)})
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/o/r/issues/1/comments", strings.NewReader(big))
	if err := p.sanitizeAgentWriteProse(req, "quality", false); err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(req.Body)
	if string(got) != big {
		t.Fatal("oversized body was altered or truncated")
	}
}

// A read error is returned so the relay can drop the exchange.
func TestSanitizeAgentWriteProseReadError(t *testing.T) {
	p := newTestProxy()
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/o/r/issues/1/comments", io.NopCloser(errReader{}))
	if err := p.sanitizeAgentWriteProse(req, "quality", false); err == nil {
		t.Fatal("want a read error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("boom") }

// End to end through the MITM relay: what reaches GitHub has a plain hyphen
// and a Content-Length that matches the rewritten body.
func TestProxyHTTPRewritesAgentCommentDashes(t *testing.T) {
	p := newTestProxy()
	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "quality", agent.ModeIssuesPRsMerge, agent.AgentCapabilities{})

	body := mustJSON(t, map[string]any{"body": "Reviewed the change " + em + " it looks right.\n\n```\nkeep " + em + " this\n```"})
	go func() {
		fmt.Fprintf(clientConn, "POST /repos/org/repo/issues/1/comments HTTP/1.1\r\nHost: api.github.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()

	type seen struct {
		body []byte
		cl   int64
	}
	got := make(chan seen, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(upstreamConn))
		if err != nil {
			got <- seen{}
			return
		}
		b, _ := io.ReadAll(req.Body)
		got <- seen{body: b, cl: req.ContentLength}
		resp := &http.Response{StatusCode: http.StatusCreated, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Body: http.NoBody, Request: req}
		_ = resp.Write(upstreamConn)
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	s := <-got
	if s.cl != int64(len(s.body)) {
		t.Errorf("upstream Content-Length = %d, body is %d bytes", s.cl, len(s.body))
	}
	text := decodeField(t, s.body, "body")
	if want := "Reviewed the change - it looks right.\n\n```\nkeep " + em + " this\n```"; text != want {
		t.Errorf("forwarded body = %q, want %q", text, want)
	}
	if bytes.Contains(s.body, []byte("change "+em)) {
		t.Error("prose em-dash reached upstream")
	}
}

func TestAgentWritesSwitchReadsEnv(t *testing.T) {
	t.Setenv(prosefilter.AgentWritesEnv, "false")
	if prosefilter.AgentWritesEnabled() {
		t.Fatal("switch off not honoured")
	}
	t.Setenv(prosefilter.AgentWritesEnv, "")
	if !prosefilter.AgentWritesEnabled() {
		t.Fatal("switch must default on")
	}
}

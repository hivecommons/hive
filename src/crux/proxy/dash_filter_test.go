package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testEm = "\u2014"

func filterReq(t *testing.T, method, path, ctype, body string) (*http.Request, string) {
	t.Helper()
	req := httptest.NewRequest(method, "https://api.github.com"+path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	p := &GitHubProxy{}
	p.filterOutboundDashes(req, "agent")
	b := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := req.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if int64(b.Len()) != req.ContentLength {
		t.Fatalf("ContentLength %d != body %d", req.ContentLength, b.Len())
	}
	return req, b.String()
}

func TestFilterOutboundDashesComment(t *testing.T) {
	_, got := filterReq(t, "POST", "/repos/o/r/issues/1/comments", "application/json",
		`{"body":"a `+testEm+` b\n`+"```"+`\nx `+testEm+` y\n`+"```"+`","n":1}`)
	if strings.Contains(strings.Split(got, "```")[0], testEm) || !strings.Contains(got, "a - b") {
		t.Fatalf("prose not rewritten: %s", got)
	}
	if !strings.Contains(got, "x "+testEm+" y") {
		t.Fatalf("code block altered: %s", got)
	}
}

func TestFilterOutboundDashesReviewAndGraphQL(t *testing.T) {
	_, got := filterReq(t, "POST", "/repos/o/r/pulls/1/reviews", "application/json",
		`{"comments":[{"body":"a `+testEm+` b","path":"p"}],"title":"t `+testEm+` u"}`)
	if strings.Contains(got, testEm) {
		t.Fatalf("review not rewritten: %s", got)
	}
	_, got = filterReq(t, "POST", "/graphql", "application/json",
		`{"query":"mutation{x}","variables":{"input":{"body":"a `+testEm+` b"}}}`)
	if strings.Contains(got, testEm) {
		t.Fatalf("graphql not rewritten: %s", got)
	}
}

func TestFilterOutboundDashesLeavesOthersAlone(t *testing.T) {
	for _, tc := range []struct{ method, path, ctype string }{
		{"POST", "/repos/o/r/issues/1/comments", "text/plain"},
		{"POST", "/o/r.git/git-receive-pack", "application/json"},
		{"GET", "/repos/o/r", "application/json"},
	} {
		body := `{"body":"a ` + testEm + ` b"}`
		if _, got := filterReq(t, tc.method, tc.path, tc.ctype, body); got != body {
			t.Fatalf("%v changed body: %s", tc, got)
		}
	}
	_, got := filterReq(t, "POST", "/x", "application/json", `{"other":"a `+testEm+` b"}`)
	if !strings.Contains(got, testEm) {
		t.Fatalf("non-text field altered: %s", got)
	}
}

func TestFilterOutboundDashesSwitchOff(t *testing.T) {
	t.Setenv("HIVE_PROXY_DASH_FILTER", "off")
	body := `{"body":"a ` + testEm + ` b"}`
	if _, got := filterReq(t, "POST", "/repos/o/r/issues/1/comments", "application/json", body); got != body {
		t.Fatalf("switch off changed body: %s", got)
	}
}

package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// fakeCanaryScanner stands in for the host's canary registry (pkg/ioscan):
// a planted token per agent, matched by plain substring. Encodings are the
// registry's job and are tested there; what the proxy owes the registry is
// the raw write body, verbatim, and this fake records what it was handed.
type fakeCanaryScanner struct {
	tokens map[string]string // agent -> token
	seen   []string
}

func newFakeCanaryScanner() *fakeCanaryScanner {
	return &fakeCanaryScanner{tokens: map[string]string{}}
}

func (f *fakeCanaryScanner) plant(agent string) string {
	tok := "CANARY-" + agent + "-7f3a9c"
	f.tokens[agent] = tok
	return tok
}

func (f *fakeCanaryScanner) Scan(agentName, text, source string) (crux.CanaryLeak, bool) {
	f.seen = append(f.seen, text)
	for agent, tok := range f.tokens {
		if strings.Contains(text, tok) {
			return crux.CanaryLeak{Agent: agent, Token: tok, Source: source}, true
		}
	}
	return crux.CanaryLeak{}, false
}

func TestInspectCanaryEgressDetectsGitHubWrites(t *testing.T) {
	r := newFakeCanaryScanner()
	tok := r.plant("scanner")
	var got crux.CanaryLeak
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, func(leak crux.CanaryLeak) { got = leak })
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"`+tok+`"}`))
	reason, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || !deny || reason == "" {
		t.Fatalf("got detected=%v deny=%v reason=%q, want detected deny", detected, deny, reason)
	}
	if got.Agent != "scanner" || got.Token != tok || got.Source != "github:POST /repos/org/repo/issues/1/comments" {
		t.Fatalf("callback leak = %+v", got)
	}
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), tok) {
		t.Fatalf("request body was not restored: %q", body)
	}
}

// The canary exists to catch deliberate exfiltration, and a deliberate
// exfiltrator encodes; the registry sees through encodings only if the proxy
// hands it the body exactly as the agent wrote it, so this pins that the
// scanner receives the raw body — not a parsed field, not a truncation.
func TestInspectCanaryEgressHandsScannerTheRawBody(t *testing.T) {
	r := newFakeCanaryScanner()
	r.plant("scanner")
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, nil)
	raw := `{"body":"Q0FOQVJZLXNjYW5uZXI=","title":"x"}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues", strings.NewReader(raw))
	_, deny, detected := p.inspectCanaryEgress("scanner", req)
	if detected || deny {
		t.Fatalf("got detected=%v deny=%v for a body the fake does not match", detected, deny)
	}
	if len(r.seen) != 1 || r.seen[0] != raw {
		t.Fatalf("scanner saw %q, want the raw body %q", r.seen, raw)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != raw || req.ContentLength != int64(len(raw)) {
		t.Fatalf("request body was not restored: %q (len %d)", body, req.ContentLength)
	}
}

func TestInspectCanaryEgressAuditOnlyWhenFailOpen(t *testing.T) {
	r := newFakeCanaryScanner()
	tok := r.plant("scanner")
	called := false
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, func(crux.CanaryLeak) { called = true })
	req, _ := http.NewRequest(http.MethodPatch, "https://api.github.com/repos/org/repo/pulls/2", strings.NewReader(`{"body":"`+tok+`"}`))
	_, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || deny || !called {
		t.Fatalf("got detected=%v deny=%v called=%v, want detected audit-only", detected, deny, called)
	}
}

func TestInspectCanaryEgressIgnoresBenignAndDisabled(t *testing.T) {
	r := newFakeCanaryScanner()
	r.plant("scanner")
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"hello"}`))
	_, _, detected := p.inspectCanaryEgress("scanner", req)
	if detected {
		t.Fatal("benign body detected as leak")
	}
	p.SetCanaryScanner(false, true, r, nil)
	req, _ = http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"hello"}`))
	_, _, detected = p.inspectCanaryEgress("scanner", req)
	if detected {
		t.Fatal("disabled scanner detected leak")
	}
	if len(r.seen) != 1 {
		t.Fatalf("disabled scanner was still consulted: %d scans", len(r.seen))
	}
}

func TestInspectCanaryEgressSkipsWithoutScanner(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, nil, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"hello"}`))
	if _, deny, detected := p.inspectCanaryEgress("scanner", req); detected || deny {
		t.Fatalf("got detected=%v deny=%v with no scanner installed, want pass-through", detected, deny)
	}
}

func TestInspectCanaryEgressBlocksOversizedBodies(t *testing.T) {
	r := newFakeCanaryScanner()
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(strings.Repeat("x", maxGitHubWriteBodyScan+1)))
	reason, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || !deny || !strings.Contains(reason, "too large") {
		t.Fatalf("got detected=%v deny=%v reason=%q, want oversized block", detected, deny, reason)
	}
	if len(r.seen) != 0 {
		t.Fatalf("oversized body was handed to the scanner")
	}
}

func TestInspectCanaryEgressCoversDirectPullCreatePath(t *testing.T) {
	r := newFakeCanaryScanner()
	tok := r.plant("quality")
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/pulls", strings.NewReader(`{"body":"`+tok+`"}`))
	_, deny, detected := p.inspectCanaryEgress("quality", req)
	if !detected || deny {
		t.Fatalf("got detected=%v deny=%v, want audit-only detection on hard-denied path", detected, deny)
	}
}

func TestInspectCanaryEgressCoversGitCommitRESTAndBlocksOpaquePush(t *testing.T) {
	r := newFakeCanaryScanner()
	tok := r.plant("quality")
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/git/commits", strings.NewReader(`{"message":"`+tok+`"}`))
	_, deny, detected := p.inspectCanaryEgress("quality", req)
	if !detected || !deny {
		t.Fatalf("REST git commit got detected=%v deny=%v, want fail-closed leak", detected, deny)
	}
	pushReq, _ := http.NewRequest(http.MethodPost, "https://api.github.com/org/repo.git/git-receive-pack", strings.NewReader("opaque pack"))
	reason, deny, detected := p.inspectCanaryEgress("quality", pushReq)
	if !detected || !deny || !strings.Contains(reason, "git push") {
		t.Fatalf("git push got detected=%v deny=%v reason=%q, want fail-closed block", detected, deny, reason)
	}
}

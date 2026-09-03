package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/ioscan"
)

func TestInspectCanaryEgressDetectsGitHubWrites(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var got ioscan.CanaryLeak
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) { got = leak })
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(`{"body":"`+c.Token+`"}`))
	reason, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || !deny || reason == "" {
		t.Fatalf("got detected=%v deny=%v reason=%q, want detected deny", detected, deny, reason)
	}
	if got.Agent != "scanner" || got.Source == "" {
		t.Fatalf("callback leak = %+v", got)
	}
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), c.Token) {
		t.Fatalf("request body was not restored: %q", body)
	}
}

func TestInspectCanaryEgressAuditOnlyWhenFailOpen(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	called := false
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, func(ioscan.CanaryLeak) { called = true })
	req, _ := http.NewRequest(http.MethodPatch, "https://api.github.com/repos/org/repo/pulls/2", strings.NewReader(`{"body":"`+c.Token+`"}`))
	_, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || deny || !called {
		t.Fatalf("got detected=%v deny=%v called=%v, want detected audit-only", detected, deny, called)
	}
}

func TestInspectCanaryEgressIgnoresBenignAndDisabled(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	if _, err := r.Add("scanner"); err != nil {
		t.Fatalf("Add: %v", err)
	}
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
}

func TestInspectCanaryEgressBlocksOversizedBodies(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/issues/1/comments", strings.NewReader(strings.Repeat("x", maxGitHubWriteBodyScan+1)))
	reason, deny, detected := p.inspectCanaryEgress("scanner", req)
	if !detected || !deny || !strings.Contains(reason, "too large") {
		t.Fatalf("got detected=%v deny=%v reason=%q, want oversized block", detected, deny, reason)
	}
}

func TestInspectCanaryEgressCoversDirectPullCreatePath(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	c, err := r.Add("quality")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, false, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/pulls", strings.NewReader(`{"body":"`+c.Token+`"}`))
	_, deny, detected := p.inspectCanaryEgress("quality", req)
	if !detected || deny {
		t.Fatalf("got detected=%v deny=%v, want audit-only detection on hard-denied path", detected, deny)
	}
}

func TestInspectCanaryEgressCoversGitCommitRESTAndBlocksOpaquePush(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	c, err := r.Add("quality")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	p := &GitHubProxy{logger: slog.Default()}
	p.SetCanaryScanner(true, true, r, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/repos/org/repo/git/commits", strings.NewReader(`{"message":"`+c.Token+`"}`))
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

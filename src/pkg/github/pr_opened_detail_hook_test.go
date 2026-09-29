package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSetPROpenedDetailHook_NilClientAndClear(t *testing.T) {
	var nilClient *Client
	nilClient.SetPROpenedDetailHook(func(PROpenedDetail) {}) // must not panic
	nilClient.SetPROpenedDetailHook(nil)

	c := testClient(t, "http://127.0.0.1:1")
	c.SetPROpenedDetailHook(func(PROpenedDetail) {})
	if h := c.prOpenedDetailHook.Load(); h == nil || *h == nil {
		t.Fatal("SetPROpenedDetailHook(fn) did not install the hook")
	}
	c.SetPROpenedDetailHook(nil)
	if h := c.prOpenedDetailHook.Load(); h != nil {
		t.Fatal("SetPROpenedDetailHook(nil) did not remove the hook")
	}
}

// The detail hook carries what the PR follow-up pointer needs beyond the
// plain hook: the published body and the request's handoff summary.
func TestSetPROpenedDetailHook_FiresWithBodyAndHandoff(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	calls := make(chan PROpenedDetail, 1)
	c.SetPROpenedDetailHook(func(d PROpenedDetail) { calls <- d })

	handoff := &PRHandoff{Why: "nil deref on empty config", Files: []string{"src/a.go"}}
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/detail-1", Title: "[scanner] fix: thing", Body: "## Why\nnil deref\n\nFixes #1", Agent: "scanner", Handoff: handoff}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	select {
	case d := <-calls:
		if d.Agent != "scanner" || d.Repo != "o/r" || d.Number != 42 || d.URL == "" {
			t.Errorf("detail = %+v, want agent=scanner repo=o/r number=42 and a URL", d)
		}
		if !strings.Contains(d.Body, "nil deref") {
			t.Errorf("detail body = %q, want the published body", d.Body)
		}
		if d.Handoff == nil || d.Handoff.Why != handoff.Why || len(d.Handoff.Files) != 1 {
			t.Errorf("detail handoff = %+v, want the request's summary", d.Handoff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PR opened but the detail hook never fired")
	}
}

func TestPRHandoffIsZero(t *testing.T) {
	var nilHandoff *PRHandoff
	if !nilHandoff.IsZero() || !(&PRHandoff{Why: "  "}).IsZero() {
		t.Fatal("nil and blank handoffs are zero")
	}
	for _, h := range []*PRHandoff{{Why: "a"}, {Approach: "a"}, {Rejected: "a"}, {Repro: "a"}, {Files: []string{"a"}}} {
		if h.IsZero() {
			t.Fatalf("%+v reported zero", h)
		}
	}
}

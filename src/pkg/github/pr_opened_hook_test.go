package github

// Tests for the two 0%-covered pkg/github PR-request helpers:
//   - prRequestPolicyError.Error (pr_request_claims.go)
//   - Client.SetPROpenedHook (pr_request_watcher.go)
// plus the watcher's hook-firing branch (fires once per NEW PR, not on reuse,
// and never after the hook is removed with nil).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPRRequestPolicyError_ErrorAndReason(t *testing.T) {
	policyErr := &prRequestPolicyError{reason: "title claims tests but diff has none"}
	if got := policyErr.Error(); got != "title claims tests but diff has none" {
		t.Errorf("Error() = %q, want the reason verbatim", got)
	}

	// prRequestPolicyReason unwraps through fmt.Errorf %w chains.
	wrapped := fmt.Errorf("validating claims: %w", policyErr)
	reason, ok := prRequestPolicyReason(wrapped)
	if !ok || reason != "title claims tests but diff has none" {
		t.Errorf("prRequestPolicyReason(wrapped) = (%q, %v), want (reason, true)", reason, ok)
	}

	// A non-policy error is not a policy mismatch.
	if reason, ok := prRequestPolicyReason(errors.New("api: 502")); ok || reason != "" {
		t.Errorf("prRequestPolicyReason(plain error) = (%q, %v), want (\"\", false)", reason, ok)
	}
	if _, ok := prRequestPolicyReason(nil); ok {
		t.Error("prRequestPolicyReason(nil) reported a policy reason")
	}
}

func TestSetPROpenedHook_NilClientNoPanic(t *testing.T) {
	var c *Client
	c.SetPROpenedHook(func(agent, repo string, number int, url string) {}) // must not panic
	c.SetPROpenedHook(nil)                                                 // must not panic
}

func TestSetPROpenedHook_SetAndClear(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	c.SetPROpenedHook(func(agent, repo string, number int, url string) {})
	if hook := c.prOpenedHook.Load(); hook == nil || *hook == nil {
		t.Fatal("SetPROpenedHook(fn) did not install the hook")
	}

	c.SetPROpenedHook(nil)
	if hook := c.prOpenedHook.Load(); hook != nil {
		t.Fatal("SetPROpenedHook(nil) did not remove the hook")
	}
}

// hookCall captures one PROpenedHook invocation.
type hookCall struct {
	agent  string
	repo   string
	number int
	url    string
}

// End-to-end: an installed hook fires (asynchronously) when the watcher opens
// a NEW PR, with the request's agent/repo and the created PR number.
func TestSetPROpenedHook_FiresOnNewPR(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	calls := make(chan hookCall, 1)
	c.SetPROpenedHook(func(agent, repo string, number int, url string) {
		calls <- hookCall{agent: agent, repo: repo, number: number, url: url}
	})

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/hook-1", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	// The hook is dispatched on its own goroutine; join it with a timeout.
	select {
	case call := <-calls:
		if call.agent != "scanner" || call.repo != "o/r" || call.number != 42 || call.url == "" {
			t.Errorf("hook called with %+v, want agent=scanner repo=o/r number=42 and a URL", call)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PR opened but hook never fired")
	}
	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
}

// Reuse path: when an open PR for the head already exists (AlreadyExisted),
// the hook must NOT fire — progress surfaces only learn about NEW PRs.
func TestSetPROpenedHook_SilentOnReusedPR(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "scanner/hook-dup", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	calls := make(chan hookCall, 1)
	c.SetPROpenedHook(func(agent, repo string, number int, url string) {
		calls <- hookCall{agent: agent, repo: repo, number: number, url: url}
	})

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/hook-dup", Title: "[scanner] fix: dup", Body: "Fixes #2", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("expected 0 PRs created (dedupe), got %d", created)
	}
	select {
	case call := <-calls:
		t.Errorf("hook fired for a reused PR: %+v", call)
	case <-time.After(100 * time.Millisecond):
		// correct: no hook call for AlreadyExisted
	}
}

// A removed hook (SetPROpenedHook(nil)) must not fire even when a new PR opens.
func TestSetPROpenedHook_RemovedHookDoesNotFire(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	calls := make(chan hookCall, 1)
	c.SetPROpenedHook(func(agent, repo string, number int, url string) {
		calls <- hookCall{agent: agent, repo: repo, number: number, url: url}
	})
	c.SetPROpenedHook(nil)

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/hook-off", Title: "[scanner] fix: off", Body: "Fixes #3", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
	select {
	case call := <-calls:
		t.Errorf("removed hook fired: %+v", call)
	case <-time.After(100 * time.Millisecond):
		// correct: cleared hook stays silent
	}
}

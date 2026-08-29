package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/ioscan"
)

func TestCreatePRBlocksCanaryWhenFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("quality")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	called := false
	c := &Client{client: gh.NewClient(nil), org: "org"}
	c.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) {
		called = true
		if leak.Agent != "quality" || !strings.HasPrefix(leak.Source, "hive-open-pr:") {
			t.Fatalf("leak = %+v", leak)
		}
	})
	_, err = c.CreatePR(context.Background(), "org/repo", "quality/fix", "main", "fix", "body "+cny.Token)
	if err == nil || !strings.Contains(err.Error(), "ioscan canary leak") {
		t.Fatalf("CreatePR error = %v, want canary block", err)
	}
	if !called {
		t.Fatal("leak callback was not called")
	}
}

// TestCreateIssueCommentBlocksCanaryWhenFailClosed pins the kubestellar/hive
// #4960 invariant directly: CreateIssueComment must refuse a canary body the
// same way CreateIssue/CreatePR do. Without the fix, CreateIssueComment never
// calls scanCanaryText at all, so this test fails (the comment "posts") the
// moment the fix is reverted — it does not merely exercise the code path.
func TestCreateIssueCommentBlocksCanaryWhenFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	posted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"org/repo"})
	called := false
	c.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) {
		called = true
		if leak.Agent != "scanner" || !strings.HasPrefix(leak.Source, "hive-comment:") {
			t.Fatalf("leak = %+v", leak)
		}
	})

	err = c.CreateIssueComment(context.Background(), "org/repo", 7, "exfil payload "+cny.Token)
	if err == nil || !strings.Contains(err.Error(), "ioscan canary leak") {
		t.Fatalf("CreateIssueComment error = %v, want canary block", err)
	}
	if !called {
		t.Fatal("leak callback was not called")
	}
	if posted {
		t.Fatal("comment was posted to GitHub despite fail-closed canary block — exfiltration succeeded")
	}
}

// TestCreateIssueAndCreateIssueCommentAgreeOnCanaryFailClosed proves parity
// between the two write paths the issue-request watcher dispatches the SAME
// agent-supplied body to (kind "issue" vs kind "comment"): an identical
// canary-carrying body must be refused through both, not just one.
func TestCreateIssueAndCreateIssueCommentAgreeOnCanaryFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	body := "exfil payload " + cny.Token

	// kind "issue"
	var created, commented int
	issueServer := newIssueMockServer(t, "", &created, nil, &commented)
	defer issueServer.Close()
	issueClient := newTestClient(t, issueServer, "org", []string{"org/repo"})
	issueClient.SetCanaryScanner(true, true, r, nil)
	_, issueErr := issueClient.CreateIssue(context.Background(), "org/repo", "title", body, nil)
	if issueErr == nil || !strings.Contains(issueErr.Error(), "ioscan canary leak") {
		t.Fatalf("CreateIssue error = %v, want canary block", issueErr)
	}
	if created != 0 {
		t.Fatal("CreateIssue: issue was created despite fail-closed canary block")
	}

	// kind "comment" — the same body, through CreateIssueComment.
	commentServer := newIssueMockServer(t, "", nil, nil, &commented)
	defer commentServer.Close()
	commentClient := newTestClient(t, commentServer, "org", []string{"org/repo"})
	commentClient.SetCanaryScanner(true, true, r, nil)
	commentErr := commentClient.CreateIssueComment(context.Background(), "org/repo", 7, body)
	if commentErr == nil || !strings.Contains(commentErr.Error(), "ioscan canary leak") {
		t.Fatalf("CreateIssueComment error = %v, want canary block (parity with CreateIssue)", commentErr)
	}
	if commented != 0 {
		t.Fatal("CreateIssueComment: comment was posted despite fail-closed canary block — the bypass kubestellar/hive#4960 describes")
	}
}

// TestCreateIssueCommentCanaryFailOpenMatchesCreateIssue pins that, with
// fail-closed OFF, CreateIssueComment behaves like CreateIssue in the same
// mode: the leak is still detected and reported (onLeak fires, a warning is
// logged) but the write is NOT blocked — this is the documented
// fail-open contract, not a guess.
func TestCreateIssueCommentCanaryFailOpenMatchesCreateIssue(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	body := "exfil payload " + cny.Token

	// kind "issue", fail-closed OFF: CreateIssue succeeds and still reports.
	var created int
	issueServer := newIssueMockServer(t, "", &created, nil, nil)
	defer issueServer.Close()
	issueClient := newTestClient(t, issueServer, "org", []string{"org/repo"})
	issueLeakSeen := false
	issueClient.SetCanaryScanner(true, false, r, func(ioscan.CanaryLeak) { issueLeakSeen = true })
	if _, err := issueClient.CreateIssue(context.Background(), "org/repo", "title", body, nil); err != nil {
		t.Fatalf("CreateIssue with fail-closed off: %v", err)
	}
	if created != 1 {
		t.Fatalf("CreateIssue with fail-closed off: created = %d, want 1 (write must proceed)", created)
	}
	if !issueLeakSeen {
		t.Fatal("CreateIssue with fail-closed off: leak was not reported even though detection must still run")
	}

	// kind "comment", fail-closed OFF: same contract — write proceeds, leak reported.
	posted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	commentClient := newTestClient(t, server, "org", []string{"org/repo"})
	commentLeakSeen := false
	commentClient.SetCanaryScanner(true, false, r, func(ioscan.CanaryLeak) { commentLeakSeen = true })
	if err := commentClient.CreateIssueComment(context.Background(), "org/repo", 7, body); err != nil {
		t.Fatalf("CreateIssueComment with fail-closed off: %v", err)
	}
	if !posted {
		t.Fatal("CreateIssueComment with fail-closed off: comment was not posted — write must proceed like CreateIssue does")
	}
	if !commentLeakSeen {
		t.Fatal("CreateIssueComment with fail-closed off: leak was not reported even though detection must still run")
	}
}

// TestPostAdvisoryDigestBlocksCanaryWhenFailClosed covers the secondary
// exfiltration channel named in kubestellar/hive#4960: the advisory digest
// aggregates agent-sourced finding text and must honor the same fail-closed
// contract as the primary write paths.
func TestPostAdvisoryDigestBlocksCanaryWhenFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	posted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/issues/10/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		posted = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"org/repo"})
	called := false
	c.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) {
		called = true
		if leak.Agent != "scanner" || !strings.HasPrefix(leak.Source, "hive-advisory-digest:") {
			t.Fatalf("leak = %+v", leak)
		}
	})

	err = c.PostAdvisoryDigest(context.Background(), "org/repo", 10, "## 🐝 Advisory Digest\nexfil "+cny.Token)
	if err == nil || !strings.Contains(err.Error(), "ioscan canary leak") {
		t.Fatalf("PostAdvisoryDigest error = %v, want canary block", err)
	}
	if !called {
		t.Fatal("leak callback was not called")
	}
	if posted {
		t.Fatal("advisory digest was posted to GitHub despite fail-closed canary block — exfiltration succeeded")
	}
}

// TestReviewRequestBlocksCanaryWhenFailClosed covers the review-request
// watcher: a PR review body is agent-supplied text posted straight to GitHub
// (ReviewRequest.Body -> PullRequests.CreateReview) with no canary scan prior
// to this fix, the same exfiltration shape kubestellar/hive#4960 describes
// for issue comments.
func TestReviewRequestBlocksCanaryWhenFailClosed(t *testing.T) {
	r := ioscan.NewCanaryRegistry("")
	cny, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	posted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/pulls/5/reviews", func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":1,"state":"COMMENTED"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	reviewRequestDirForTest = dir
	defer func() { reviewRequestDirForTest = "" }()

	c := newTestClient(t, server, "org", []string{"org/repo"})
	called := false
	c.SetCanaryScanner(true, true, r, func(leak ioscan.CanaryLeak) {
		called = true
		if leak.Agent != "scanner" || !strings.HasPrefix(leak.Source, "hive-review:") {
			t.Fatalf("leak = %+v", leak)
		}
	})
	c.reviewAuthz = func(agent string, fileUID int) error { return nil }

	req := ReviewRequest{Repo: "org/repo", Number: 5, Event: "comment", Body: "exfil " + cny.Token, Agent: "scanner"}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reqPath := filepath.Join(dir, "req.json")
	if err := os.WriteFile(reqPath, b, 0o644); err != nil {
		t.Fatalf("write request: %v", err)
	}

	c.ProcessReviewRequestsOnce(context.Background())

	if posted {
		t.Fatal("review was posted to GitHub despite fail-closed canary block — exfiltration succeeded")
	}
	if !called {
		t.Fatal("leak callback was not called")
	}

	resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	rb, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading result file: %v", err)
	}
	var resp ReviewResponse
	if err := json.Unmarshal(rb, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if resp.OK {
		t.Fatal("review result reported OK despite canary block")
	}
	if !strings.Contains(resp.Error, "ioscan canary leak") {
		t.Fatalf("review result error = %q, want ioscan canary leak", resp.Error)
	}
}

package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestMergeRequestPolicyNilClientIsSafe(t *testing.T) {
	var c *Client
	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{"o/r": true})
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})
	c.cacheBaseBranchProtection("o/r:main", true, time.Now())
	if c.mergeRequestAllowsUnprotectedBase("o/r") || c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("nil client must not report policy opt-ins")
	}
	if protected, ok := c.cachedBaseBranchProtection("o", "r", "main"); ok || protected {
		t.Fatalf("nil cachedBaseBranchProtection = (%v,%v), want (false,false)", protected, ok)
	}
	if err := c.verifyMergeRequestBaseProtected(context.Background(), "o/r", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil verifyMergeRequestBaseProtected = %v, want ErrNoGitHubClient", err)
	}
}

func TestMergeRequestPolicyRepoSetsNormalizeAndClear(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, nil)

	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{" r ": true, "ignored": false})
	if !c.mergeRequestAllowsUnprotectedBase("o/r") || !c.mergeRequestAllowsUnprotectedBase("r") {
		t.Fatal("allow_unprotected_base should match bare and qualified repo spellings")
	}
	if c.mergeRequestAllowsUnprotectedBase("o/ignored") {
		t.Fatal("false entries must not opt a repo in")
	}

	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"O/R": true})
	if !c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("no_ci_ok matching should be case-insensitive")
	}

	c.SetMergeRequestAllowUnprotectedBaseRepos(nil)
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{" ": true})
	if c.mergeRequestAllowsUnprotectedBase("o/r") || c.mergeRequestAllowsNoCI("o/r") {
		t.Fatal("nil/blank repo sets should clear opt-ins")
	}
}

func TestMergeRequestBaseProtectionCacheAvoidsRepeatedBranchProtectionLookup(t *testing.T) {
	var protectionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"head":{"sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main/protection"):
			protectionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"url": "protected"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, nil)
	for i := 0; i < 2; i++ {
		if err := c.verifyMergeRequestBaseProtected(context.Background(), "o/r", 42); err != nil {
			t.Fatalf("verify protected base attempt %d: %v", i+1, err)
		}
	}
	if got := protectionCalls.Load(); got != 1 {
		t.Fatalf("branch protection should be cached, got %d lookups", got)
	}
	if protected, ok := c.cachedBaseBranchProtection("o", "r", "main"); !ok || !protected {
		t.Fatalf("cachedBaseBranchProtection = (%v,%v), want (true,true)", protected, ok)
	}
}

func TestMergeRequestBaseProtectionEmptyBaseAndSentinel(t *testing.T) {
	if !isGitHubNotFound(gh.ErrBranchNotProtected) || isGitHubNotFound(errors.New("other")) {
		t.Fatal("isGitHubNotFound should recognize only GitHub not-found/unprotected errors")
	}
	if !isGitHubNotFound(&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}) {
		t.Fatal("isGitHubNotFound should recognize GitHub 404 responses")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"head":{"sha":"abc"},"base":{"ref":""}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, nil)
	if err := c.verifyMergeRequestBaseProtected(context.Background(), "o/r", 42); err == nil || !strings.Contains(err.Error(), "no base branch") {
		t.Fatalf("expected empty-base refusal, got %v", err)
	}

	c.cacheBaseBranchProtection("o/r:old", true, time.Now().Add(-2*baseBranchProtectionCacheTTL))
	if protected, ok := c.cachedBaseBranchProtection("o", "r", "old"); ok || protected {
		t.Fatalf("expired cachedBaseBranchProtection = (%v,%v), want (false,false)", protected, ok)
	}
}

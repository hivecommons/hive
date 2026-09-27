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

func TestMergeRequestBaseProtectionCacheAvoidsRepeatedBranchLookup(t *testing.T) {
	var branchCalls atomic.Int32
	var protectionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"head":{"sha":"abc"},"base":{"ref":"main"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main"):
			branchCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": true})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main/protection"):
			protectionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Resource not accessible by integration"})
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
	if got := branchCalls.Load(); got != 1 {
		t.Fatalf("branch protection should be cached from branch lookup, got %d lookups", got)
	}
	if got := protectionCalls.Load(); got != 0 {
		t.Fatalf("legacy branch-protection endpoint should not be called, got %d calls", got)
	}
	if protected, ok := c.cachedBaseBranchProtection("o", "r", "main"); !ok || !protected {
		t.Fatalf("cachedBaseBranchProtection = (%v,%v), want (true,true)", protected, ok)
	}
}

func TestMergeRequestBaseProtectionUnprotectedBaseRequiresOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"head":{"sha":"abc"},"base":{"ref":"main"}}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": false})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, nil)
	if err := c.verifyMergeRequestBaseProtected(context.Background(), "o/r", 42); err == nil || !strings.Contains(err.Error(), "allow_unprotected_base") {
		t.Fatalf("expected unprotected-base refusal, got %v", err)
	}
	if protected, ok := c.cachedBaseBranchProtection("o", "r", "main"); !ok || protected {
		t.Fatalf("cachedBaseBranchProtection = (%v,%v), want (false,true)", protected, ok)
	}

	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{"o/r": true})
	if err := c.verifyMergeRequestBaseProtected(context.Background(), "o/r", 42); err != nil {
		t.Fatalf("allow_unprotected_base should permit unprotected base, got %v", err)
	}
}

func TestMergeRequestBaseProtectionEmptyBaseAndExpiredCache(t *testing.T) {
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

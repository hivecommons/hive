package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closingLinkFixture serves the GraphQL lookup EnsureClosingIssueLink issues
// and records any addCloseIssueReferences mutation it receives.
type closingLinkFixture struct {
	defaultBranch string
	base          string
	prState       string
	issueState    string
	linked        []int
	lookups       atomic.Int32
	mutations     atomic.Int32
	lastMutation  map[string]any
}

func (f *closingLinkFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "addCloseIssueReferences") {
			f.mutations.Add(1)
			f.lastMutation = req.Variables
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"addCloseIssueReferences": map[string]any{"issue": map[string]any{"number": 7}},
			}})
			return
		}
		f.lookups.Add(1)
		nodes := []map[string]any{}
		for _, n := range f.linked {
			nodes = append(nodes, map[string]any{"number": n, "repository": map[string]any{"nameWithOwner": "hivecommons/hive"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"repository": map[string]any{
				"defaultBranchRef": map[string]any{"name": f.defaultBranch},
				"pullRequest": map[string]any{
					"id":                      "PR_1",
					"state":                   f.prState,
					"baseRefName":             f.base,
					"closingIssuesReferences": map[string]any{"nodes": nodes},
				},
				"issue": map[string]any{"id": "I_7", "state": f.issueState},
			},
		}})
	}
}

func newClosingLinkClient(t *testing.T, f *closingLinkFixture) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
}

func TestEnsureClosingIssueLink(t *testing.T) {
	tests := []struct {
		name          string
		fixture       *closingLinkFixture
		wantLinked    bool
		wantAlready   bool
		wantSkipped   string
		wantMutations int32
	}{
		{
			name:          "non-default base without link sends the mutation",
			fixture:       &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "OPEN", issueState: "OPEN"},
			wantLinked:    true,
			wantMutations: 1,
		},
		{
			name:        "already linked is a no-op",
			fixture:     &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "OPEN", issueState: "OPEN", linked: []int{7}},
			wantAlready: true,
		},
		{
			name:        "default-branch PR is left to GitHub",
			fixture:     &closingLinkFixture{defaultBranch: "v5", base: "v5", prState: "OPEN", issueState: "OPEN"},
			wantSkipped: "default-branch",
		},
		{
			name:        "closed PR is not linked",
			fixture:     &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "CLOSED", issueState: "OPEN"},
			wantSkipped: "pr-not-open",
		},
		{
			name:        "closed issue is not linked",
			fixture:     &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "OPEN", issueState: "CLOSED"},
			wantSkipped: "issue-not-open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.fixture
			c := newClosingLinkClient(t, f)
			got, err := c.EnsureClosingIssueLink(context.Background(), "hivecommons/hive", 1, 7)
			if err != nil {
				t.Fatalf("EnsureClosingIssueLink: %v", err)
			}
			if got.Linked != tt.wantLinked || got.AlreadyLinked != tt.wantAlready || got.Skipped != tt.wantSkipped {
				t.Fatalf("result = %+v, want linked=%v already=%v skipped=%q", got, tt.wantLinked, tt.wantAlready, tt.wantSkipped)
			}
			if f.mutations.Load() != tt.wantMutations {
				t.Fatalf("mutations = %d, want %d", f.mutations.Load(), tt.wantMutations)
			}
			if tt.wantLinked {
				if f.lastMutation["issue"] != "I_7" {
					t.Fatalf("mutation issue id = %v, want I_7", f.lastMutation["issue"])
				}
				prs, _ := f.lastMutation["prs"].([]any)
				if len(prs) != 1 || prs[0] != "PR_1" {
					t.Fatalf("mutation prs = %v, want [PR_1]", f.lastMutation["prs"])
				}
			}
			// A second call for the same pair is answered from the memo.
			again, err := c.EnsureClosingIssueLink(context.Background(), "hivecommons/hive", 1, 7)
			if err != nil {
				t.Fatalf("second EnsureClosingIssueLink: %v", err)
			}
			if again.Skipped != "cached" {
				t.Fatalf("second call = %+v, want cached", again)
			}
			if f.lookups.Load() != 1 {
				t.Fatalf("lookups = %d, want 1 (memoised)", f.lookups.Load())
			}
		})
	}
}

func TestEnsureClosingIssueLinkLookupErrorIsNotCached(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": "NOT_FOUND", "message": "nope"}}})
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	for i := 0; i < 2; i++ {
		if _, err := c.EnsureClosingIssueLink(context.Background(), "hivecommons/hive", 1, 7); err == nil {
			t.Fatal("expected lookup error")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (errors must not be memoised)", calls.Load())
	}
}

// TestCreatePR_LinksClosingIssuesOnNonDefaultBase covers the relay hook: a PR
// opened against v6 with `Fixes #7` gets addCloseIssueReferences sent.
func TestCreatePR_LinksClosingIssuesOnNonDefaultBase(t *testing.T) {
	f := &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "OPEN", issueState: "OPEN"}
	graphql := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			graphql(w, r)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "html_url": "https://github.com/hivecommons/hive/pull/1", "user": map[string]any{"login": "hive[bot]"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	res, err := c.CreatePR(context.Background(), "hive", "fix/thing", "v6", "✨ fix", "Fixes #7\n\nRefs #8")
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if res.Number != 1 {
		t.Fatalf("number = %d, want 1", res.Number)
	}
	if f.mutations.Load() != 1 {
		t.Fatalf("mutations = %d, want 1 (only the Fixes reference is linked)", f.mutations.Load())
	}
}

// TestSyncIssuePRClaimLabels_LinksStrongOpenClaims covers the enumeration
// hook for PRs opened outside the relay.
func TestSyncIssuePRClaimLabels_LinksStrongOpenClaims(t *testing.T) {
	f := &closingLinkFixture{defaultBranch: "v5", base: "v6", prState: "OPEN", issueState: "OPEN"}
	graphql := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			graphql(w, r)
		case strings.Contains(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	now := time.Now()
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{
		{Repo: "hivecommons/hive", Issue: 7, PRNumber: 1, PRRepo: "hivecommons/hive", PRState: "open", Closing: true, ObservedAt: now, FirstObservedAt: now},
		{Repo: "hivecommons/hive", Issue: 8, PRNumber: 2, PRRepo: "hivecommons/hive", PRState: "open", Reference: true, ObservedAt: now, FirstObservedAt: now},
	}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{
		{Repo: "hivecommons/hive", Number: 7},
		{Repo: "hivecommons/hive", Number: 8},
	}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.mutations.Load() != 1 {
		t.Fatalf("mutations = %d, want 1 (only the strong claim is linked)", f.mutations.Load())
	}
}

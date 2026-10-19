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
	// mutationErr, when set, makes addCloseIssueReferences answer with a
	// GraphQL error carrying this message.
	mutationErr  string
	lookups      atomic.Int32
	mutations    atomic.Int32
	lastMutation map[string]any
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
			if f.mutationErr != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"type": "FORBIDDEN", "message": f.mutationErr}}})
				return
			}
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
			// hivecommons/hive#12005: a v5 PR whose claim came from the
			// branch name has no keyword for GitHub to derive, so hive
			// links it exactly as it does a release-line PR.
			name:          "default-branch PR GitHub did not link sends the mutation",
			fixture:       &closingLinkFixture{defaultBranch: "v5", base: "v5", prState: "OPEN", issueState: "OPEN"},
			wantLinked:    true,
			wantMutations: 1,
		},
		{
			name:        "default-branch PR GitHub already linked is a no-op",
			fixture:     &closingLinkFixture{defaultBranch: "v5", base: "v5", prState: "OPEN", issueState: "OPEN", linked: []int{7}},
			wantAlready: true,
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

// TestEnsureClosingIssueLinkMutationErrorIsNotCached covers the retry
// contract the PR-claim sweep relies on: when the lookup succeeds but
// addCloseIssueReferences fails, the pair is not memoised, so the next
// sweep looks it up and tries the mutation again.
func TestEnsureClosingIssueLinkMutationErrorIsNotCached(t *testing.T) {
	f := &closingLinkFixture{defaultBranch: "v5", base: "v5", prState: "OPEN", issueState: "OPEN"}
	f.mutationErr = "Resource not accessible by integration"
	c := newClosingLinkClient(t, f)
	for i := 0; i < 2; i++ {
		_, err := c.EnsureClosingIssueLink(context.Background(), "hivecommons/hive", 1, 7)
		if err == nil || !strings.Contains(err.Error(), "addCloseIssueReferences") || !strings.Contains(err.Error(), f.mutationErr) {
			t.Fatalf("call %d: err = %v, want addCloseIssueReferences error carrying the GraphQL message", i, err)
		}
	}
	if f.lookups.Load() != 2 || f.mutations.Load() != 2 {
		t.Fatalf("lookups=%d mutations=%d, want 2/2 (failures must not be memoised)", f.lookups.Load(), f.mutations.Load())
	}
	f.mutationErr = ""
	res, err := c.EnsureClosingIssueLink(context.Background(), "hivecommons/hive", 1, 7)
	if err != nil || !res.Linked {
		t.Fatalf("after recovery: res=%+v err=%v, want linked", res, err)
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
// hook for PRs opened outside the relay: every claim that earns the
// covered-by-PR label is linked — closing-keyword and branch-name claims
// alike, on the default branch too (hivecommons/hive#12005) — except a
// Reference claim, whose author deliberately did not ask to close the issue.
func TestSyncIssuePRClaimLabels_LinksStrongOpenClaims(t *testing.T) {
	f := &closingLinkFixture{defaultBranch: "v5", base: "v5", prState: "OPEN", issueState: "OPEN"}
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
		// Branch-name claim: neither Closing nor Reference.
		{Repo: "hivecommons/hive", Issue: 9, PRNumber: 3, PRRepo: "hivecommons/hive", PRState: "open", ObservedAt: now, FirstObservedAt: now},
	}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{
		{Repo: "hivecommons/hive", Number: 7},
		{Repo: "hivecommons/hive", Number: 8},
		{Repo: "hivecommons/hive", Number: 9},
	}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.mutations.Load() != 2 {
		t.Fatalf("mutations = %d, want 2 (closing and branch-name claims linked; reference claim not)", f.mutations.Load())
	}
	for _, issue := range result.Issues.Items {
		if !HasCoveredByPRLabel(issue.Labels) {
			t.Fatalf("issue #%d labels = %v, want covered-by-PR on every claimed issue", issue.Number, issue.Labels)
		}
	}
	// Three lookups: one per linked pair, plus the reference claim's
	// retraction check (hivecommons/hive#12144), which finds no link and
	// is memoised like the rest. A second sweep is answered from the memo:
	// no new lookups or mutations.
	if f.lookups.Load() != 3 {
		t.Fatalf("lookups = %d, want 3 (two ensure lookups, one retraction check)", f.lookups.Load())
	}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.lookups.Load() != 3 || f.mutations.Load() != 2 {
		t.Fatalf("after second sweep lookups=%d mutations=%d, want 3/2 (memoised)", f.lookups.Load(), f.mutations.Load())
	}
}

type issueStagePRLinkFixture struct {
	body          string
	base          string
	defaultBranch string
	author        string
	state         string
	patches       atomic.Int32
	patchedBody   string
}

func (f *issueStagePRLinkFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/pulls/1" {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.patches.Add(1)
			f.patchedBody = req.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "body": req.Body})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if f.state == "" {
			f.state = "OPEN"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"repository": map[string]any{
				"defaultBranchRef": map[string]any{"name": f.defaultBranch},
				"pullRequest": map[string]any{
					"state":       f.state,
					"baseRefName": f.base,
					"body":        f.body,
					"url":         "https://github.com/hivecommons/hive/pull/1",
					"author":      map[string]any{"login": f.author},
				},
			},
		}})
	}
}

func newIssueStagePRLinkClient(t *testing.T, f *issueStagePRLinkFixture) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	c.SetAppBotLogin("hive[bot]")
	return c
}

func TestEnsureIssueStagePRLinkEditsHiveDefaultBranchPR(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "## Fix\n\nImplements the retry.", base: "v5", defaultBranch: "v5", author: "hive[bot]"}
	c := newIssueStagePRLinkClient(t, f)
	got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1"})
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if !got.EditedBody || got.StageComment != "" || f.patches.Load() != 1 {
		t.Fatalf("result=%+v patches=%d, want one body edit and no issue comment", got, f.patches.Load())
	}
	if !strings.HasSuffix(f.patchedBody, "\n\nFixes #7") {
		t.Fatalf("patched body %q must append Fixes trailer", f.patchedBody)
	}
}

func TestEnsureIssueStagePRLinkLeavesPartialPRBodyAlone(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "## Fix\n\nPart of #7; leaves follow-up work open.", base: "v5", defaultBranch: "v5", author: "hive[bot]"}
	c := newIssueStagePRLinkClient(t, f)
	got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1", Reference: true})
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if got.EditedBody || f.patches.Load() != 0 || got.Skipped != "partial-reference" {
		t.Fatalf("result=%+v patches=%d, want part-of no-op", got, f.patches.Load())
	}
	if !strings.Contains(got.StageComment, "PR #1 covers this issue") || !strings.Contains(got.StageComment, "GitHub will not show") {
		t.Fatalf("stage comment = %q", got.StageComment)
	}
}

func TestEnsureIssueStagePRLinkRetrofitsRefsOnlyHivePR(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "## Fix\n\nRefs #7 — implemented by this Hive PR.", base: "v5", defaultBranch: "v5", author: "app/hive"}
	c := newIssueStagePRLinkClient(t, f)
	c.SetAppBotLogin("hive[bot]")
	got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1", Reference: true})
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if !got.EditedBody || f.patches.Load() != 1 {
		t.Fatalf("result=%+v patches=%d, want refs retrofit edit", got, f.patches.Load())
	}
	if !strings.Contains(f.patchedBody, "Fixes #7 — implemented") || !strings.Contains(f.patchedBody, issueLinkFixedMarker) {
		t.Fatalf("patched body = %q", f.patchedBody)
	}

	f2 := &issueStagePRLinkFixture{body: f.patchedBody, base: "v5", defaultBranch: "v5", author: "app/hive"}
	c2 := newIssueStagePRLinkClient(t, f2)
	c2.SetAppBotLogin("hive[bot]")
	again, err := c2.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1", Reference: true})
	if err != nil {
		t.Fatalf("second EnsureIssueStagePRLink: %v", err)
	}
	if again.EditedBody || f2.patches.Load() != 0 || again.Skipped != "already-closing" {
		t.Fatalf("second result=%+v patches=%d, want marker to prevent a second rewrite", again, f2.patches.Load())
	}
}

func TestEnsureIssueStagePRLinkExplainsNonDefaultBranch(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "## Fix\n\nImplements the retry.", base: "v6", defaultBranch: "v5", author: "hive[bot]"}
	c := newIssueStagePRLinkClient(t, f)
	got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1"})
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if got.EditedBody || f.patches.Load() != 0 || got.Skipped != "non-default-base" {
		t.Fatalf("result=%+v patches=%d, want non-default no-op", got, f.patches.Load())
	}
	if !strings.Contains(got.StageComment, "targets `v6`") || !strings.Contains(got.StageComment, "repository default branch") {
		t.Fatalf("stage comment = %q", got.StageComment)
	}
}

func TestEnsureIssueStagePRLinkNeverEditsHumanPR(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "## Fix\n\nRefs #7 — implemented by this PR.", base: "v5", defaultBranch: "v5", author: "octocat"}
	c := newIssueStagePRLinkClient(t, f)
	got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, PRURL: "https://github.com/hivecommons/hive/pull/1", Reference: true})
	if err != nil {
		t.Fatalf("EnsureIssueStagePRLink: %v", err)
	}
	if got.EditedBody || f.patches.Load() != 0 || got.Skipped != "partial-reference" {
		t.Fatalf("result=%+v patches=%d, want human no-op", got, f.patches.Load())
	}
}

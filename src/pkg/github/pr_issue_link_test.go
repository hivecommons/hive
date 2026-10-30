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
	// Four lookups: one per linked pair, the reference claim's PR-body
	// retrofit check, plus that claim's retraction check
	// (hivecommons/hive#12144), which finds no link and is memoised like the
	// rest. A second sweep is answered from the memo:
	// no new lookups or mutations.
	if f.lookups.Load() != 4 {
		t.Fatalf("lookups = %d, want 4 (two ensure lookups, one retrofit check, one retraction check)", f.lookups.Load())
	}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.lookups.Load() != 5 || f.mutations.Load() != 2 {
		t.Fatalf("after second sweep lookups=%d mutations=%d, want 5/2 (link/retraction memoised; retrofit rechecked)", f.lookups.Load(), f.mutations.Load())
	}
}

func TestSyncIssuePRClaimLabels_RetrofitsHiveRefsClaim(t *testing.T) {
	f := &issueStagePRLinkFixture{body: "Refs #7 — implemented by this PR.", base: "v5", defaultBranch: "v5", author: "app/hivecommons-hive"}
	prLink := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql") || r.Method == http.MethodPatch:
			prLink(w, r)
		case strings.Contains(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	c.SetAppBotLogin("hivecommons-hive[bot]")
	now := time.Now()
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{{Repo: "hivecommons/hive", Issue: 7, PRNumber: 1, PRRepo: "hivecommons/hive", PRURL: "https://github.com/hivecommons/hive/pull/1", PRState: "open", Reference: true, ObservedAt: now, FirstObservedAt: now}}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 7}}

	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if f.patches.Load() != 1 {
		t.Fatalf("patches = %d, want one PR body retrofit", f.patches.Load())
	}
	if !strings.Contains(f.patchedBody, "Fixes #7") || !strings.Contains(f.patchedBody, issueLinkFixedMarker) {
		t.Fatalf("patched body %q must rewrite Refs to Fixes and stamp marker", f.patchedBody)
	}
	if !HasCoveredByPRLabel(result.Issues.Items[0].Labels) {
		t.Fatalf("labels = %v, want covered-by-PR", result.Issues.Items[0].Labels)
	}
}

func TestRewriteRefsIssueLinkToClosingPreservesWatcherDowngrade(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"bare-reference", "Refs #7", true},
		{"recorded-decision", "Refs #7\n" + prDecisionHeadMarkerLine("head", prDecisionChangesRequested), false},
		{"watcher-downgrade", "Refs #7" + downgradeNote("unfinished-delegated-work"), false},
		{"watcher-downgrade-multiple-refs", "Refs #7" + downgradeNote("unfinished-delegated-work") + "\nRefs #8", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ok := rewriteRefsIssueLinkToClosing(tc.body, "hivecommons/hive", 7)
			if ok != tc.want {
				t.Fatalf("rewrite = %v (%q), want %v", ok, body, tc.want)
			}
			if !ok && body != "" {
				t.Fatalf("refused rewrite returned body %q", body)
			}
		})
	}
}

type issueStagePRLinkFixture struct {
	lastEditedAt   string
	decisionBody   string
	decisionAuthor string
	body           string
	base           string
	defaultBranch  string
	author         string
	state          string
	patches        atomic.Int32
	patchedBody    string
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
					"state":        f.state,
					"baseRefName":  f.base,
					"body":         f.body,
					"lastEditedAt": f.lastEditedAt,
					"comments":     map[string]any{"nodes": []map[string]any{{"body": f.decisionBody, "author": map[string]string{"login": f.decisionAuthor}}}},
					"url":          "https://github.com/hivecommons/hive/pull/1",
					"author":       map[string]any{"login": f.author},
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

// testsuitePR1057Body is projectbluefin/testsuite#1057 as it was merged: a
// partial `Refs #1023` note and the `Closes #1023` the contributor wrapper
// appended (hivecommons/hive#15025).
var testsuitePR1057Body = "**Summary:** Operating system update checks should not report success when they skip a required test. This change makes targeted update runs fail if any required update or rollback check did not finish successfully. Runs without an update target keep their existing exploratory skip behavior.\n\n---\n\n## Changes\n\n- Add a lifecycle `after_all` verdict guard when the existing `TARGET_IMAGE` environment variable is non-empty; no workflow changes or new inputs.\n- Identify required scenarios by the existing mandatory staging assertion. Require each scenario and all its steps to pass, and reject an empty required set.\n- Inspect the complete parsed runner model, including scenarios and whole features excluded by tag filters. A hook failure propagates through the existing Behave exit-code verdict.\n- Preserve optional capability skips and untargeted manual/exploratory behavior. Targeted manual runs receive the strict guard too.\n- Add unit and real Behave subprocess tests next to the digest-evidence tests. Update the bootc skill with the guard, its activation, and its limits.\n\n## Scope and remaining qualification work\n\nRefs #1023 — partial: implements the latest maintainer comment's testsuite-only skipped-required-scenario guard. Independently pinned source/candidate fixtures, intended-candidate verification, removal of switch pre-staging, external caller updates, and hosted positive/negative qualification evidence remain open pending the second-image prerequisite in #706 and the deferred workflow work.\n\nThis guard validates required scenarios in the loaded feature model; it does not certify fixture provenance or replace caller responsibility to load full required coverage. Existing customization assertions and migration scenarios are unchanged. Untargeted runs do not establish targeted qualification.\n\n## Validation\n\n- Lifecycle module: 80 passed, including actual Behave exit-code checks for runtime skips, a tag-filtered required feature, successful required completion, and untargeted skips.\n- Full unit suite: 2,338 passed, 1 expected failure, 4 subtests passed.\n- Ruff E/F/W selection: passed.\n- Lifecycle dry-run: 33 scenarios, 365 steps, no undefined steps; scenario totals unchanged.\n- Documentation validation and skill catalog check: passed, with three existing KDE reference-name warnings.\n- Stub listing and `git diff --check`: passed. Open PR file sets checked for overlap; none overlap these three files.\n- System Python initially lacked test dependencies; validation used an isolated temporary virtual environment with the needed dependencies.\n- Context7 is unavailable on this host; consulted the official Behave API documentation for lifecycle hooks and verified behavior with real subprocess tests.\n- No hosted VM qualification was run. These are regression checks, not pinned-image upgrade/rollback evidence.\n\n— hive: backend=pi model=openai-codex/gpt-6.1-sol\n\nCloses #1023\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `887f0ddd`"

// testsuitePR1051Body is the tail of projectbluefin/testsuite#1051 before its
// author removed the wrapper-appended `Closes #1025`.
var testsuitePR1051Body = "## Acceptance status for #1025\n\n- [x] Per-profile inventory (merged in #1040). Each startup unit now also says how the lane judges it.\n- [x] Required services run in hosted QEMU with bounded deadlines, and their completion, failures and resulting state are observed (this PR).\n- [x] Flatpak initialization, remotes, apps and permissions come from the shipped path and are asserted (this PR). The pending scenarios are **not** re-enabled yet. That needs runtime evidence from this lane first.\n- [~] Homebrew system setup is exercised. Greenboot is **deferred**: automatic rollback needs a boot-counting bootloader, which direct-kernel boot doesn't have (#706). The lane reports it as not qualified and never passes it.\n- [x] Negative control: the `broken` job must go red for a deliberately broken required unit. Unit tests cover a skipped oneshot, a missing app, a wrong branch, an unknown origin, a missing remote and a permission override that isn't in effect.\n- [x] Actual hosted runs ([run 40702252373](https://github.com/projectbluefin/testsuite/actions/runs/40702252373)) on `bluefin:stable`:\n  - `broken` passed: the lane went red for the broken `NetworkManager-wait-online.service`.\n  - `candidate` failed: every unit passed, but `io.github.kolunmi.Bazaar` was not installed. The image ships `/usr/share/flatpak/preinstall.d/bazaar.preinstall` and no `remotes.d` file, so `flatpak preinstall` had no remote, logged \"Nothing to do\" and exited 0.\n  - 67226b4 makes the lane wait as the inventory promises. A missing remote or app is `pending` until `flatpak-preinstall.service`'s 1800 s deadline (or `--final`), and a unit failure is `pending` while `is-system-running` still says `starting`. Run 40702252373 was judged final at t=114s while the guest was still starting. Wrong branches, unknown origins and override gaps still fail at once. The `plan` step now sets `pipefail`.\n  - c354724 records the remote as installer-provided (`flatpak_remote: installer` on `flatpak-preinstall.service`). The evidence: the `install-flatpaks.sh` that the projectbluefin/iso Justfile writes, and the `install-system-flatpaks` ujust recipe, both run `flatpak remote-add --system flathub`. If the image ships no remote and none is configured, the lane reports the remote and each missing ref as `not qualified`. If a remote is shipped or configured, the full checks still run. Replaying that run's real `evidence.txt` through the new code, `candidate` moves from fail to pending on `brew-setup`, and `broken` still fails as expected.\n\nStill open after this PR: re-enabling the pending scenarios using this lane's evidence, user-session first-login paths (`ublue-user-setup`, `brew-preinstall`, Brewfile Flatpaks; these belong with #1026), greenboot (#706), and any consumer or promotion wiring (#1021, #1022).\n\n## Evidence\n\n- `pytest tests/unit/test_startup_lane.py tests/unit/test_startup_service_inventory.py`: 62 passed.\n- Full gate in a venv with CI deps:\n  - `ruff check tests/ --select E,F,W --ignore E501`: clean.\n  - `pytest -n auto tests/unit/`: 2341 passed, 1 xfailed.\n  - `validate_docs.py`: passed.\n  - `generate_skill_index.py --check`: in sync.\n- `actionlint` on the new workflow: only shellcheck `info` notes about intentional word-splitting of `SSH_COMMON` and unit lists. I fixed its one warning (an unused variable).\n- No `.feature` files or scenario totals changed, so there was no dry-run and no coverage snapshot to regenerate.\n- Note: `scripts/vm_validation_plan.py` routes the new files fail-closed (`scripts/*` → smoke and common, and the new workflow → the full set). I left it alone because #1050 edits that file.\n\nNote on trailers: the commit carries `Assisted-by: Claude Opus 5.5 via Claude Code` and a Claude co-author line instead of the Copilot co-author line in AGENTS.md, because Copilot did not author this.\n\nRefs #1025. Partial: scenario re-enables, greenboot (#706) and release wiring remain open.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n— hive: backend=claude model=claude-opus-5-5\n\nCloses #1025\n\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `73d9ff57`"

func TestClosingContradictions(t *testing.T) {
	const repo = "projectbluefin/testsuite"
	for _, tc := range []struct {
		name string
		body string
		want []int
	}{
		{name: "testsuite#1057 partial refs and closes", body: testsuitePR1057Body, want: []int{1023}},
		{name: "testsuite#1051 partial refs and closes", body: testsuitePR1051Body, want: []int{1025}},
		{name: "refs only", body: "Refs #1023 — partial: two items remain open."},
		{name: "closes only", body: "Fixes the guard.\n\nCloses #1023"},
		{name: "bare refs next to closes (#10826)", body: "Refs #1023 (reporter confirmation required)\n\nCloses #1023"},
		{name: "partial note about another issue", body: "Refs #706 — partial, the rest is deferred.\n\nCloses #1023"},
		{name: "stays-open phrase", body: "This leaves #1023 open for the follow-up.\n\nFixes: #1023", want: []int{1023}},
		{name: "does not close phrase", body: "This PR does not close #1023.\n\nResolves #1023", want: []int{1023}},
		{name: "stays-open phrase names a longer number", body: "#10234 stays open.\n\nCloses #1023"},
		{name: "part of with follow-up", body: "Part of #1023; the docs are a follow-up.\nCloses #1023", want: []int{1023}},
		{name: "partial refs inside a code span", body: "Write `Refs #1023 — partial` when work remains.\n\nCloses #1023"},
		{name: "closing keyword inside a fence", body: "Refs #1023 — partial.\n\n```\nCloses #1023\n```"},
		{name: "cross-repo close", body: "Refs other/repo#1023 — partial.\n\nCloses other/repo#1023"},
		{name: "same-repo spelled out", body: "Refs #1023 — partial.\n\nCloses projectbluefin/testsuite#1023", want: []int{1023}},
		{name: "prose closing gap is not a GitHub close", body: "Refs #1023 — partial.\n\nThis fixes the flake in #1023"},
		{name: "empty", body: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClosingContradictions(tc.body, repo)
			if len(got) != len(tc.want) {
				t.Fatalf("ClosingContradictions = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ClosingContradictions = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestEnsureIssueStagePRLinkPreservesOwnerReference(t *testing.T) {
	for _, tc := range []struct {
		name, edited, actor string
		wantEdit            bool
	}{
		{name: "recorded decision", actor: "hive[bot]"},
		{name: "body edited after opening", edited: "2026-10-28T01:15:00Z"},
		{name: "untrusted pasted decision marker", actor: "outsider", wantEdit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &issueStagePRLinkFixture{body: "Refs #7 — further expectations remain open on the issue.", base: "v5", defaultBranch: "v5", author: "hive[bot]", lastEditedAt: tc.edited, decisionAuthor: tc.actor, decisionBody: prDecisionHeadMarkerLine("head", prDecisionChangesRequested)}
			c := newIssueStagePRLinkClient(t, f)
			got, err := c.EnsureIssueStagePRLink(context.Background(), "hivecommons/hive", 7, &IssueClaimContext{PRNumber: 1, Reference: true})
			if err != nil || got.EditedBody != tc.wantEdit || (f.patches.Load() > 0) != tc.wantEdit {
				t.Fatalf("result=%+v err=%v patches=%d", got, err, f.patches.Load())
			}
		})
	}
}

func TestGitHubClosingRefs(t *testing.T) {
	const repo = "projectbluefin/testsuite"
	for _, tc := range []struct {
		name string
		text string
		want []int
	}{
		// projectbluefin/testsuite#1054's commit closed the open PR #1036.
		{name: "prose keyword before a number", text: "Apply the same fix #1036 makes for Files/apps.\n\nCloses #1044", want: []int{1036, 1044}},
		{name: "colon form", text: "Fixes: #12", want: []int{12}},
		{name: "conventional commit type is not a keyword", text: "fix(smoke): stop over-waiting (#1054)"},
		{name: "conventional commit type then keyword", text: "fix: the thing (closes #60)", want: []int{60}},
		{name: "bounded prose gap is not a GitHub close", text: "Fixes the bug in #12"},
		{name: "other repository", text: "Closes other/repo#12"},
		{name: "same repository spelled out, any case", text: "closes ProjectBluefin/Testsuite#12", want: []int{12}},
		{name: "code span and URL", text: "`fix #12` and https://example.com/fix #13x"},
		{name: "de-duplicated", text: "Fixes #12\nCloses #12", want: []int{12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := GitHubClosingRefs(tc.text, repo)
			if len(got) != len(tc.want) {
				t.Fatalf("GitHubClosingRefs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("GitHubClosingRefs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestBodySaysIssueStaysOpenNumberBoundary(t *testing.T) {
	const repo = "acme/widget"
	if !bodySaysIssueStaysOpen("This does not close #12.", repo, 12) {
		t.Fatal("does not close #12 should keep #12 open")
	}
	if bodySaysIssueStaysOpen("This does not close #123.", repo, 12) {
		t.Fatal("does not close #123 says nothing about #12")
	}
	if !bodySaysIssueStaysOpen("Leaving #12 open until the docs land.", repo, 12) {
		t.Fatal("leaving #12 open should keep #12 open")
	}
}

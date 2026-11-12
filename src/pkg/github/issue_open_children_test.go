package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A parent whose sub-issues are still open is not likely-done, whatever a
// merged PR said about it. These tests pin the three places in this package
// that read the relation for that purpose: the in-memory claim annotation,
// the PR-claim label sync, and the live re-read the contribute hub makes
// before it closes an issue. The last two tests pin the webhook patch, which
// used to replace a parent's row with one that had no children at all.

func mergedClosingClaim(issue, pr int) IssueClaim {
	now := time.Now()
	return IssueClaim{
		Repo: "acme/widgets", Issue: issue, PRNumber: pr, PRRepo: "acme/widgets",
		Closing: true, MergedPR: true, MergedAt: now.Add(-time.Hour), ObservedAt: now, FirstObservedAt: now.Add(-time.Hour),
	}
}

func TestAnnotateIssueWithClaim_ParentWithOpenChildIsNotLikelyDone(t *testing.T) {
	parent := Issue{Repo: "acme/widgets", Number: 7, Labels: []string{LikelyDoneLabel}, Children: &IssueChildren{Total: 3, Completed: 0}}
	annotateIssueWithClaim(&parent, mergedClosingClaim(7, 45))
	if issueHasLabel(parent.Labels, LikelyDoneLabel) {
		t.Fatalf("labels = %v, want no likely-done on a parent with open sub-issues", parent.Labels)
	}
	if len(parent.LinkedPRs) != 1 || parent.LinkedPRs[0].Number != 45 {
		t.Fatalf("linked PRs = %+v, want the merged PR still linked", parent.LinkedPRs)
	}

	// Unchanged: all children closed, or no children, is likely-done as before.
	for _, kids := range []*IssueChildren{nil, {Total: 3, Completed: 3}} {
		issue := Issue{Repo: "acme/widgets", Number: 7, Children: kids}
		annotateIssueWithClaim(&issue, mergedClosingClaim(7, 45))
		if !issueHasLabel(issue.Labels, LikelyDoneLabel) {
			t.Fatalf("children %+v: labels = %v, want likely-done as before", kids, issue.Labels)
		}
	}

	// Unchanged: a merged reference-only claim is partially-addressed, with
	// or without open children.
	ref := mergedClosingClaim(7, 45)
	ref.Closing, ref.Reference = false, true
	partial := Issue{Repo: "acme/widgets", Number: 7, Children: &IssueChildren{Total: 3, Completed: 0}}
	annotateIssueWithClaim(&partial, ref)
	if !issueHasLabel(partial.Labels, PartiallyAddressedLabel) || issueHasLabel(partial.Labels, LikelyDoneLabel) {
		t.Fatalf("labels = %v, want partially-addressed only", partial.Labels)
	}
}

// claimLabelServer records the label writes the claim sync makes on
// acme/widgets#7 and fails the test on any other GitHub call.
type claimLabelServer struct {
	mu      sync.Mutex
	added   []string
	removed []string
}

func (s *claimLabelServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/labels"):
			raw, _ := io.ReadAll(r.Body)
			s.added = append(s.added, string(raw))
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			s.removed = append(s.removed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncIssuePRClaimLabels_ParentWithOpenChildGetsNoLikelyDone(t *testing.T) {
	cases := []struct {
		name        string
		labels      []string
		children    *IssueChildren
		wantAdded   int
		wantRemoved int
		wantLabel   bool
	}{
		{name: "open child: likely-done is not added", children: &IssueChildren{Total: 3, Completed: 0}},
		{name: "open child: likely-done carried from before comes off", labels: []string{LikelyDoneLabel}, children: &IssueChildren{Total: 3, Completed: 1}, wantRemoved: 1},
		{name: "no children: likely-done added as before", wantAdded: 1, wantLabel: true},
		{name: "all children closed: likely-done added as before", children: &IssueChildren{Total: 3, Completed: 3}, wantAdded: 1, wantLabel: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &claimLabelServer{}
			client := NewClientForTest(fake.start(t).URL, "acme", []string{"widgets"}, testLogger())
			ledger := NewClaimLedger("", testLogger())
			ledger.Reconcile([]IssueClaim{mergedClosingClaim(7, 45)}, true)
			result := &ActionableResult{Issues: IssueResult{Items: []Issue{{
				Repo: "acme/widgets", Number: 7, Labels: tc.labels, Children: tc.children,
			}}, Count: 1}}

			SyncIssuePRClaimLabels(context.Background(), client, result, ledger, testLogger())

			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.added) != tc.wantAdded {
				t.Fatalf("label adds = %v, want %d", fake.added, tc.wantAdded)
			}
			if tc.wantAdded == 1 && !strings.Contains(fake.added[0], LikelyDoneLabel) {
				t.Fatalf("label added = %q, want %s", fake.added[0], LikelyDoneLabel)
			}
			if len(fake.removed) != tc.wantRemoved {
				t.Fatalf("label removals = %v, want %d", fake.removed, tc.wantRemoved)
			}
			if tc.wantRemoved == 1 && !strings.Contains(fake.removed[0], "likely-done") {
				t.Fatalf("label removed = %q, want likely-done", fake.removed[0])
			}
			if got := issueHasLabel(result.Issues.Items[0].Labels, LikelyDoneLabel); got != tc.wantLabel {
				t.Fatalf("in-memory labels = %v, want likely-done=%v", result.Issues.Items[0].Labels, tc.wantLabel)
			}
		})
	}
}

func TestLiveIssueChildren(t *testing.T) {
	prev := SplitParentsPath
	SplitParentsPath = filepath.Join(t.TempDir(), "split-parents.json")
	t.Cleanup(func() { SplitParentsPath = prev })
	// The relay split #9 into #3 and #4 but GitHub shows no native link.
	for _, child := range []int{3, 4} {
		if err := RecordSplitParent("", "acme/widgets", child, 9, "scanner"); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		issue := func(number int, state string, summary *IssueChildren) {
			payload := map[string]any{"number": number, "state": state, "title": "t"}
			if summary != nil {
				payload["sub_issues_summary"] = summary
			}
			_ = json.NewEncoder(w).Encode(payload)
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/issues/7":
			issue(7, "open", &IssueChildren{Total: 3, Completed: 1})
		case "/repos/acme/widgets/issues/8":
			issue(8, "open", nil)
		case "/repos/acme/widgets/issues/9":
			issue(9, "open", &IssueChildren{})
		case "/repos/acme/widgets/issues/3":
			issue(3, "open", nil)
		case "/repos/acme/widgets/issues/4":
			issue(4, "closed", nil)
		case "/repos/acme/widgets/issues/10":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	ctx := context.Background()

	native, err := c.LiveIssueChildren(ctx, "acme/widgets", 7)
	if err != nil || native != (IssueChildren{Total: 3, Completed: 1}) || native.Open() != 2 {
		t.Fatalf("native summary = %+v err=%v, want 3 total / 1 completed", native, err)
	}
	leaf, err := c.LiveIssueChildren(ctx, "acme/widgets", 8)
	if err != nil || leaf != (IssueChildren{}) {
		t.Fatalf("leaf = %+v err=%v, want no children", leaf, err)
	}
	split, err := c.LiveIssueChildren(ctx, "acme/widgets", 9)
	if err != nil || split != (IssueChildren{Total: 2, Completed: 1}) {
		t.Fatalf("ledger children = %+v err=%v, want 2 total / 1 completed", split, err)
	}
	if _, err := c.LiveIssueChildren(ctx, "acme/widgets", 10); err == nil {
		t.Fatal("an unreadable issue must be an error, never \"no children\"")
	}
	var nilClient *Client
	if _, err := nilClient.LiveIssueChildren(ctx, "acme/widgets", 7); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v", err)
	}
}

// The webhook re-read of a parent keeps the sub-issue summary GitHub put on
// the same payload, so the row that replaces the enumerated one is still a
// parent.
func TestFetchWebhookItemSnapshot_CarriesSubIssueSummary(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	resetRESTAccountingForTest(t, clock.now)
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/issues/21":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 21, "title": "Widget pipeline", "state": "open", "body": "steps",
				"user":   map[string]any{"login": "alice"},
				"labels": []map[string]any{{"name": "kind/bug"}}, "created_at": clock.now.Add(-time.Hour).Format(time.RFC3339),
				"sub_issues_summary": map[string]any{"total": 3, "completed": 0, "percent_completed": 0},
			})
		case "/repos/acme/widgets/issues/22":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 22, "title": "Plain bug", "state": "open", "body": "steps",
				"user":   map[string]any{"login": "alice"},
				"labels": []map[string]any{{"name": "kind/bug"}}, "created_at": clock.now.Add(-time.Hour).Format(time.RFC3339),
				"sub_issues_summary": map[string]any{"total": 0, "completed": 0, "percent_completed": 0},
			})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	ctx := context.Background()

	parent, err := c.FetchWebhookItemSnapshot(ctx, "widgets", 21, false)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Patch.Bucket != WebhookBucketActionable || parent.Patch.Issue == nil {
		t.Fatalf("parent patch = %+v, want an actionable issue row", parent.Patch)
	}
	if kids := parent.Patch.Issue.Children; kids == nil || *kids != (IssueChildren{Total: 3, Completed: 0}) {
		t.Fatalf("parent row children = %+v, want 3 total / 0 completed", kids)
	}
	leaf, err := c.FetchWebhookItemSnapshot(ctx, "widgets", 22, false)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Patch.Issue == nil || leaf.Patch.Issue.Children != nil {
		t.Fatalf("leaf patch = %+v, want an issue row with no children", leaf.Patch)
	}
}

// A webhook patch whose read names no children keeps the ones the replaced
// row carried (a ledger- or marker-resolved relation is not on the issue
// payload); a read that does name them wins.
func TestApplyWebhookItem_KeepsParentChildren(t *testing.T) {
	r := patchFixture()
	for i := range r.Issues.Items {
		if r.Issues.Items[i].Number == 1 {
			r.Issues.Items[i].Children = &IssueChildren{Total: 2, Completed: 0}
		}
	}
	childrenOfRow := func(number int) *IssueChildren {
		t.Helper()
		for _, it := range r.Issues.Items {
			if it.Number == number {
				return it.Children
			}
		}
		t.Fatalf("issue #%d is not in the actionable set: %+v", number, r.Issues.Items)
		return nil
	}

	relabelled := &Issue{Repo: "org/repo", Number: 1, Title: "one, relabelled"}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 1, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: relabelled}}) {
		t.Fatal("expected a change")
	}
	if kids := childrenOfRow(1); kids == nil || *kids != (IssueChildren{Total: 2, Completed: 0}) {
		t.Fatalf("children after a re-read that names none = %+v, want the previous 2 total / 0 completed", kids)
	}
	if relabelled.Children != nil {
		t.Fatal("the caller's patch must not be mutated")
	}

	fresh := &Issue{Repo: "org/repo", Number: 1, Title: "one", Children: &IssueChildren{Total: 2, Completed: 2}}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 1, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: fresh}})
	if kids := childrenOfRow(1); kids == nil || !kids.AllClosed() {
		t.Fatalf("children after a re-read that names them = %+v, want the fresh 2 / 2", kids)
	}

	// An issue that never had children gains none.
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 2, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &Issue{Repo: "org/repo", Number: 2, Title: "two"}}})
	if kids := childrenOfRow(2); kids != nil {
		t.Fatalf("leaf children = %+v, want none", kids)
	}
}

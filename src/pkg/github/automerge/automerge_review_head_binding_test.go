package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The PR's commit graph for the binding tests. f166 is the content the owner
// approved at 2026-11-04T01:17Z. merge1 merges the base (b2) into it and
// leaves the PR's diff unchanged; content1 is a content commit made after
// the approval; merge2 merges the base into content1.
type bindingGraph struct {
	mu      sync.Mutex
	reviews []map[string]any
	calls   int
}

func bindingCommit(sha, date string, parents ...string) map[string]any {
	ps := make([]map[string]string, 0, len(parents))
	for _, p := range parents {
		ps = append(ps, map[string]string{"sha": p})
	}
	return map[string]any{"sha": sha, "parents": ps, "commit": map[string]any{"committer": map[string]string{"date": date}}}
}

var (
	prDiff      = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "sha": "blobA", "additions": 3, "deletions": 1, "changes": 4, "patch": "@@ -1 +1 @@"}}
	changedDiff = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "sha": "blobB", "additions": 9, "deletions": 1, "changes": 10, "patch": "@@ -1 +9 @@"}}
)

func newBindingAPI(t *testing.T, g *bindingGraph) *httptest.Server {
	t.Helper()
	commits := map[string]map[string]any{
		"f166":     bindingCommit("f166", "2026-11-02T23:49:05Z", "p0"),
		"merge1":   bindingCommit("merge1", "2026-11-05T19:50:10Z", "f166", "b2"),
		"content1": bindingCommit("content1", "2026-11-05T10:00:00Z", "f166"),
		"merge2":   bindingCommit("merge2", "2026-11-05T19:50:10Z", "content1", "b2"),
	}
	compares := map[string]map[string]any{
		"f166":     {"merge_base_commit": map[string]string{"sha": "b1"}, "files": prDiff},
		"merge1":   {"merge_base_commit": map[string]string{"sha": "b2"}, "files": prDiff},
		"content1": {"merge_base_commit": map[string]string{"sha": "b1"}, "files": changedDiff},
		"merge2":   {"merge_base_commit": map[string]string{"sha": "b2"}, "files": changedDiff},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.calls++
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/repos/acme/widget/pulls/7/reviews":
			_ = json.NewEncoder(w).Encode(g.reviews)
		case p == "/repos/acme/widget/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "base": map[string]string{"ref": "main"}})
		case strings.HasPrefix(p, "/repos/acme/widget/commits/"):
			if c, ok := commits[strings.TrimPrefix(p, "/repos/acme/widget/commits/")]; ok {
				_ = json.NewEncoder(w).Encode(c)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(p, "/repos/acme/widget/compare/main..."):
			if c, ok := compares[strings.TrimPrefix(p, "/repos/acme/widget/compare/main...")]; ok {
				_ = json.NewEncoder(w).Encode(c)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func ownerReview(commit, submitted string) map[string]any {
	r := map[string]any{"id": 11, "state": "APPROVED", "commit_id": commit, "user": map[string]string{"login": "danathar"}}
	if submitted != "" {
		r["submitted_at"] = submitted
	}
	return r
}

// projectbluefin/utah#1078, utah-packages#877, common#1622: a review submitted
// before the head commit existed counts only when the head adds nothing but
// base merges to the content it approved.
func TestApprovalBindsToApprovedContent(t *testing.T) {
	const approvedAt = "2026-11-04T01:17:18Z"
	for _, tc := range []struct {
		name        string
		review      map[string]any
		head        string
		wantOK      bool
		wantCarried bool
	}{
		{name: "submitted after the head commit", review: ownerReview("f166", approvedAt), head: "f166", wantOK: true},
		{name: "non-merge head dated after the review (pusher clock ahead)", review: ownerReview("f166", "2026-11-02T23:40:00Z"), head: "f166", wantOK: true},
		{name: "GitHub moved it onto a merge-only head", review: ownerReview("merge1", approvedAt), head: "merge1", wantOK: true, wantCarried: true},
		{name: "GitHub moved it onto a head with a content delta", review: ownerReview("merge2", approvedAt), head: "merge2"},
		{name: "on the previous head, base merge since", review: ownerReview("f166", approvedAt), head: "merge1", wantOK: true, wantCarried: true},
		{name: "on the previous head, content push since", review: ownerReview("f166", approvedAt), head: "content1"},
		{name: "on the previous head, content then base merge", review: ownerReview("f166", approvedAt), head: "merge2"},
		{name: "no submitted_at keeps the commit_id rule", review: ownerReview("merge2", ""), head: "merge2", wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &bindingGraph{reviews: []map[string]any{tc.review}}
			api := newBindingAPI(t, g)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			c.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "danathar") })
			login, carried, ok := c.latestTrustedHumanApprovalReach(context.Background(), "acme", "widget", 7, tc.head)
			if ok != tc.wantOK || carried != tc.wantCarried {
				t.Fatalf("approval = %q carried=%v ok=%v, want ok=%v carried=%v", login, carried, ok, tc.wantOK, tc.wantCarried)
			}
		})
	}
}

// The answer for a fixed review and head is read from GitHub once.
func TestApprovalBindingIsMemoised(t *testing.T) {
	g := &bindingGraph{reviews: []map[string]any{ownerReview("merge1", "2026-11-04T01:17:18Z")}}
	api := newBindingAPI(t, g)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "danathar") })
	c.latestTrustedHumanApprovalReach(context.Background(), "acme", "widget", 7, "merge1")
	g.mu.Lock()
	first := g.calls
	g.mu.Unlock()
	c.latestTrustedHumanApprovalReach(context.Background(), "acme", "widget", 7, "merge1")
	g.mu.Lock()
	defer g.mu.Unlock()
	if second := g.calls - first; second != 1 {
		t.Fatalf("second lookup made %d calls, want 1 (the review list only)", second)
	}
}

func TestWorkflowApprovalCommentSaysCarried(t *testing.T) {
	carried := workflowApprovalComment("9d8ceabe7d2a", "Danathar", workflowApprovalVerdictNone, true)
	if !strings.Contains(carried, "carried over a base merge") || !strings.Contains(carried, "PR diff unchanged") || strings.Contains(carried, "approved head `") {
		t.Fatalf("carried comment:\n%s", carried)
	}
	if plain := workflowApprovalComment("9d8ceabe7d2a", "Danathar", workflowApprovalVerdictNone, false); !strings.Contains(plain, "approved head `9d8ceabe7d2a`") || strings.Contains(plain, "carried") {
		t.Fatalf("plain comment:\n%s", plain)
	}
}

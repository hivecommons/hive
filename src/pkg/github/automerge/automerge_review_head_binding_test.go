package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
		// backdated is a content push the hive first listed at 2026-11-05
		// 10:00Z whose committer date claims 2026-11-03; merge3 merges the
		// base into it.
		"backdated": bindingCommit("backdated", "2026-11-03T00:00:00Z", "f166"),
		"merge3":    bindingCommit("merge3", "2026-11-05T19:50:10Z", "backdated", "b2"),
		// evilmerge merges a side branch (not the base) with a backdated date.
		"evilmerge": bindingCommit("evilmerge", "2026-11-03T00:00:00Z", "f166", "side"),
	}
	compares := map[string]map[string]any{
		"f166":      {"merge_base_commit": map[string]string{"sha": "b1"}, "files": prDiff},
		"merge1":    {"merge_base_commit": map[string]string{"sha": "b2"}, "files": prDiff},
		"content1":  {"merge_base_commit": map[string]string{"sha": "b1"}, "files": changedDiff},
		"merge2":    {"merge_base_commit": map[string]string{"sha": "b2"}, "files": changedDiff},
		"backdated": {"merge_base_commit": map[string]string{"sha": "b1"}, "files": changedDiff},
		"merge3":    {"merge_base_commit": map[string]string{"sha": "b2"}, "files": changedDiff},
		"evilmerge": {"merge_base_commit": map[string]string{"sha": "b1"}, "files": changedDiff},
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
	// The hive listed f166 as the head shortly after its push, and the
	// backdated content push only on 11-05.
	seen := map[string]string{"f166": "2026-11-02T23:50:00Z", "content1": "2026-11-05T10:01:00Z", "backdated": "2026-11-05T10:00:00Z", "merge1": "2026-11-05T19:51:00Z", "evilmerge": "2026-11-05T11:00:00Z"}
	for _, tc := range []struct {
		name        string
		review      map[string]any
		head        string
		unseen      bool // no sightings on record (seen before the record existed)
		wantOK      bool
		wantCarried bool
	}{
		{name: "submitted after the head commit", review: ownerReview("f166", approvedAt), head: "f166", wantOK: true},
		{name: "non-merge head dated after the review (pusher clock ahead)", review: ownerReview("f166", "2026-11-02T23:40:00Z"), head: "f166", wantOK: true},
		{name: "GitHub moved it onto a merge-only head", review: ownerReview("merge1", approvedAt), head: "merge1", wantOK: true, wantCarried: true},
		{name: "moved onto a merge-only head, first sighting unknown", review: ownerReview("merge1", approvedAt), head: "merge1", unseen: true},
		{name: "backdated content push under a later base merge", review: ownerReview("merge3", approvedAt), head: "merge3"},
		{name: "backdated non-base merge head the hive saw after the review", review: ownerReview("evilmerge", approvedAt), head: "evilmerge"},
		{name: "merge head approved after the hive saw it", review: ownerReview("merge1", "2026-11-06T00:00:00Z"), head: "merge1", wantOK: true},
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
			c.headSightings = NewHeadSightings("")
			if !tc.unseen {
				for sha, at := range seen {
					c.headSightings.Sight("acme/widget", map[int]string{7: sha}, mustParseTime(t, at))
				}
			}
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

// Sightings persist across a restart and keep the first time a head was
// listed; openPullRequests records every listed head.
func TestHeadSightingsPersistFirstSeen(t *testing.T) {
	path := t.TempDir() + "/sightings.json"
	s := NewHeadSightings(path)
	first := mustParseTime(t, "2026-11-02T23:50:00Z")
	s.Sight("Acme/Widget", map[int]string{7: "f166"}, first)
	s.Sight("acme/widget", map[int]string{7: "f166"}, first.Add(time.Hour))
	reloaded := NewHeadSightings(path)
	if at, ok := reloaded.FirstSeen("acme/widget", 7, "f166"); !ok || !at.Equal(first) {
		t.Fatalf("first seen after reload = %v %v, want %v", at, ok, first)
	}
	if _, ok := reloaded.FirstSeen("acme/widget", 7, "other"); ok {
		t.Fatal("an unseen head must be unknown")
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/acme/widget/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 9, "state": "open", "head": map[string]string{"sha": "h9"}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	c.headSightings = NewHeadSightings("")
	if _, _, err := c.openPullRequests(context.Background(), "acme", "widget", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.headSightings.FirstSeen("acme/widget", 9, "h9"); !ok {
		t.Fatal("a listed head was not sighted")
	}
}

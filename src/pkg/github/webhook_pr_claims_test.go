package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#14814: a `pull_request` webhook must link the PR to the
// issues it claims the moment it lands, not on the next reconcile pass.

func webhookPRPayload(action string, pr map[string]any) []byte {
	p := map[string]any{"action": action, "repository": map[string]any{"full_name": "org/repo"}, "pull_request": pr}
	b, _ := json.Marshal(p)
	return b
}

func webhookPRObject(number int, body, author, state string) map[string]any {
	return map[string]any{
		"number":   number,
		"title":    "Fix the thing",
		"body":     body,
		"state":    state,
		"html_url": "https://github.com/org/repo/pull/" + string(rune('0'+number)),
		"user":     map[string]any{"login": author},
		"head":     map[string]any{"ref": "fix-thing", "sha": "abc123"},
	}
}

func TestHandleWebhookEventPullRequestRecordsClaims(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	var got []PRClaimDelivery
	SetPRClaimsHook(func(d PRClaimDelivery) { got = append(got, d) })
	t.Cleanup(func() { SetPRClaimsHook(nil) })

	// Pre-populate the open-PR link cache so the delivery's invalidation is observable.
	cache := c.openPRLinks()
	cache.mu.Lock()
	cache.repos["repo"] = &openPRLinkIndex{fetchedAt: clock.now}
	cache.mu.Unlock()

	res, err := c.HandleWebhookEvent("pull_request", webhookPRPayload("opened", webhookPRObject(9, "Fixes #7\nRefs #8", "alice", "open")))
	if err != nil {
		t.Fatal(err)
	}
	if !res.KnownRepo {
		t.Fatalf("result = %+v, want known repo", res)
	}
	if len(res.ClaimedIssues) != 2 || res.ClaimedIssues[0] != 7 || res.ClaimedIssues[1] != 8 {
		t.Fatalf("ClaimedIssues = %v, want [7 8]", res.ClaimedIssues)
	}
	if len(got) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(got))
	}
	d := got[0]
	if d.Repo != "repo" || d.Number != 9 || d.Action != "opened" || d.State != PRStateOpen || !d.At.Equal(clock.now) {
		t.Fatalf("delivery = %+v", d)
	}
	byIssue := map[int]IssueClaim{}
	for _, cl := range d.Claims {
		byIssue[cl.Issue] = cl
	}
	if cl := byIssue[7]; !cl.ExternalAuthor || cl.Reference || cl.PRAuthor != "alice" || cl.PRState != PRStateOpen {
		t.Fatalf("claim on #7 = %+v, want external closing claim by alice", cl)
	}
	if cl := byIssue[8]; !cl.Reference {
		t.Fatalf("claim on #8 = %+v, want reference claim", cl)
	}
	cache.mu.Lock()
	_, still := cache.repos["repo"]
	cache.mu.Unlock()
	if still {
		t.Fatal("open-PR link cache for repo must be invalidated by the delivery")
	}

	// Actions that cannot change the claims do not fire the hook.
	for _, action := range []string{"labeled", "review_requested", "assigned"} {
		if _, err := c.HandleWebhookEvent("pull_request", webhookPRPayload(action, webhookPRObject(9, "Fixes #7", "alice", "open"))); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("hook calls after no-op actions = %d, want still 1", len(got))
	}

	// Unknown repo: no hook.
	if _, err := c.HandleWebhookEvent("pull_request", []byte(`{"action":"opened","repository":{"full_name":"other/repo"},"pull_request":{"number":1,"body":"Fixes #7","state":"open"}}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("hook fired for an unmanaged repo: %d calls", len(got))
	}

	// Merged close carries the merge state.
	merged := webhookPRObject(9, "Fixes #7", "alice", "closed")
	merged["merged"] = true
	merged["merged_at"] = clock.now.Format(time.RFC3339)
	if _, err := c.HandleWebhookEvent("pull_request", webhookPRPayload("closed", merged)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].State != PRStateMerged || !got[1].MergedAt.Equal(clock.now) {
		t.Fatalf("merged delivery = %+v", got[len(got)-1])
	}

	// Unmerged close is closed.
	if _, err := c.HandleWebhookEvent("pull_request", webhookPRPayload("closed", webhookPRObject(9, "Fixes #7", "alice", "closed"))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].State != PRStateClosed {
		t.Fatalf("closed delivery = %+v", got[len(got)-1])
	}

	// No hook installed: still accepted, response still lists the claims.
	SetPRClaimsHook(nil)
	res, err = c.HandleWebhookEvent("pull_request", webhookPRPayload("opened", webhookPRObject(10, "Closes #11", "bob", "open")))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ClaimedIssues) != 1 || res.ClaimedIssues[0] != 11 {
		t.Fatalf("ClaimedIssues without hook = %v, want [11]", res.ClaimedIssues)
	}
}

func TestClaimLedgerApplyPRDelivery(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	l := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), testLogger())
	l.now = func() time.Time { return now }
	identity := HiveIdentity{AppLogin: "hive[bot]"}
	pr := func(number int, body, author string) *gh.PullRequest {
		return &gh.PullRequest{
			Number: gh.Ptr(number), Title: gh.Ptr("t"), Body: gh.Ptr(body),
			User: &gh.User{Login: gh.Ptr(author)}, State: gh.Ptr("open"),
			Head: &gh.PullRequestBranch{Ref: gh.Ptr("branch")},
		}
	}
	delivery := func(number int, body, author, state string, at time.Time) PRClaimDelivery {
		return PRClaimDelivery{Repo: "repo", Number: number, Action: "opened", State: state, At: at,
			Claims: claimsFromPR(pr(number, body, author), "repo", identity, at)}
	}

	// Open external PR: claim recorded and visible to Lookup.
	rec, rel := l.ApplyPRDelivery(delivery(100, "Fixes #1\nFixes #2", "alice", PRStateOpen, now))
	if rec != 2 || rel != 0 {
		t.Fatalf("open: recorded=%d released=%d, want 2/0", rec, rel)
	}
	c1, ok := l.Lookup("repo", 1)
	if !ok || c1.PRNumber != 100 || !c1.ExternalAuthor || c1.PRState != PRStateOpen || !c1.FirstObservedAt.Equal(now) {
		t.Fatalf("claim #1 = %+v ok=%v", c1, ok)
	}

	// Edited to drop #2: #2 released, #1 kept with FirstObservedAt intact.
	later := now.Add(5 * time.Minute)
	rec, rel = l.ApplyPRDelivery(delivery(100, "Fixes #1", "alice", PRStateOpen, later))
	if rec != 1 || rel != 1 {
		t.Fatalf("edited: recorded=%d released=%d, want 1/1", rec, rel)
	}
	if _, ok := l.Lookup("repo", 2); ok {
		t.Fatal("#2 must be released after the PR stopped naming it")
	}
	c1, _ = l.Lookup("repo", 1)
	if !c1.FirstObservedAt.Equal(now) || !c1.ObservedAt.Equal(later) {
		t.Fatalf("claim #1 after edit = %+v, want FirstObservedAt carried forward", c1)
	}

	// Another PR's claim on a different issue is untouched by this PR's deliveries.
	if rec, _ := l.ApplyPRDelivery(delivery(200, "Fixes #3", "bob", PRStateOpen, later)); rec != 1 {
		t.Fatalf("second PR recorded=%d, want 1", rec)
	}

	// Closed without merging: this PR's claims released, the other PR's kept.
	rec, rel = l.ApplyPRDelivery(delivery(100, "Fixes #1", "alice", PRStateClosed, later))
	if rec != 0 || rel != 1 {
		t.Fatalf("closed: recorded=%d released=%d, want 0/1", rec, rel)
	}
	if _, ok := l.Lookup("repo", 1); ok {
		t.Fatal("#1 must be released after the PR closed unmerged")
	}
	if _, ok := l.Lookup("repo", 3); !ok {
		t.Fatal("#3 from the other PR must survive")
	}

	// Merged: recorded as a merged claim anchored at the merge.
	d := delivery(200, "Fixes #3", "bob", PRStateMerged, later)
	d.MergedAt = later
	rec, rel = l.ApplyPRDelivery(d)
	if rec != 1 || rel != 0 {
		t.Fatalf("merged: recorded=%d released=%d, want 1/0 (open claim replaced in place)", rec, rel)
	}
	c3, ok := l.Lookup("repo", 3)
	if !ok || !c3.MergedPR || c3.PRState != PRStateMerged || !c3.MergedAt.Equal(later) {
		t.Fatalf("merged claim #3 = %+v ok=%v", c3, ok)
	}

	// Nil ledger / empty delivery: no-ops.
	var nilLedger *ClaimLedger
	if rec, rel := nilLedger.ApplyPRDelivery(d); rec != 0 || rel != 0 {
		t.Fatal("nil ledger must be a no-op")
	}
	if rec, rel := l.ApplyPRDelivery(PRClaimDelivery{}); rec != 0 || rel != 0 {
		t.Fatal("empty delivery must be a no-op")
	}
}

func TestClaimLedgerApplyPRDeliveryFiltersSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	l := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), testLogger())
	l.now = func() time.Time { return now }
	snap := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "repo", Number: 1}, {Repo: "repo", Number: 2}}, Count: 2}}

	d := PRClaimDelivery{Repo: "repo", Number: 50, Action: "opened", State: PRStateOpen, At: now,
		Claims: claimsFromPR(&gh.PullRequest{Number: gh.Ptr(50), Body: gh.Ptr("Fixes #1"),
			User: &gh.User{Login: gh.Ptr("alice")}, State: gh.Ptr("open"),
			Head: &gh.PullRequestBranch{Ref: gh.Ptr("b")}}, "repo", HiveIdentity{AppLogin: "hive[bot]"}, now)}
	l.ApplyPRDelivery(d)
	if n := FilterClaimedIssues(snap, l, nil, testLogger()); n != 1 {
		t.Fatalf("suppressed=%d, want 1", n)
	}
	if snap.Issues.Count != 1 || snap.Issues.Items[0].Number != 2 {
		t.Fatalf("snapshot after filter = %+v, want only #2", snap.Issues)
	}
}

func TestPRClaimDeliveryClaimedIssues(t *testing.T) {
	d := PRClaimDelivery{Repo: "repo", Claims: []IssueClaim{
		{Repo: "repo", Issue: 9}, {Repo: "repo", Issue: 3}, {Repo: "repo", Issue: 9},
		{Repo: "other", Issue: 1}, {Repo: "repo", Issue: 0},
	}}
	got := d.ClaimedIssues()
	if len(got) != 2 || got[0] != 3 || got[1] != 9 {
		t.Fatalf("ClaimedIssues = %v, want [3 9]", got)
	}
}

func TestIssueDispatchBlockReasonClosedIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed", "labels": []any{}})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	reason, err := c.IssueDispatchBlockReason(context.Background(), "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if reason != ClaimBlockReasonIssueClosed {
		t.Fatalf("reason = %q, want %q", reason, ClaimBlockReasonIssueClosed)
	}
}

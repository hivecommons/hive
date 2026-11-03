package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func patchFixture() *ActionableResult {
	now := time.Now()
	return &ActionableResult{
		GeneratedAt: now.Add(-10 * time.Minute),
		Issues: IssueResultFromItems([]Issue{
			{Repo: "org/repo", Number: 1, Title: "one", Labels: []string{"kind/bug"}, CreatedAt: now.Add(-time.Hour)},
			{Repo: "org/repo", Number: 2, Title: "two", CreatedAt: now.Add(-2 * time.Hour)},
		}),
		PRs:         PRResult{Count: 1, Items: []PullRequest{{Repo: "org/repo", Number: 10, Title: "pr ten", State: "open"}}},
		Hold:        HoldResult{Issues: 1, Total: 1, Items: []HoldItem{{Repo: "org/repo", Number: 3, Title: "held", Type: "issue", Labels: []string{"hold"}}}},
		TotalByRepo: map[string]RepoCounts{"org/repo": {Issues: 4, PRs: 1}},
		WorkDetailsByRepo: map[string]RepoWorkDetails{"org/repo": {
			Issues: []RepoWorkIssue{{Repo: "org/repo", Number: 4, Title: "filtered", Bucket: "filtered"}},
		}},
	}
}

// A closed issue leaves the actionable set; its repo's open count drops;
// nothing else moves. No list call is involved: the patch is pure.
func TestApplyWebhookItem_ClosedIssueRemoved(t *testing.T) {
	r := patchFixture()
	orig := r.Clone()
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 1, State: "closed"}) {
		t.Fatal("expected a change")
	}
	if r.Issues.Count != 1 || r.Issues.Items[0].Number != 2 {
		t.Fatalf("issues after close: %+v", r.Issues)
	}
	if got := r.TotalByRepo["org/repo"]; got != (RepoCounts{Issues: 3, PRs: 1}) {
		t.Fatalf("repo counts = %+v", got)
	}
	if orig.Issues.Count != 2 {
		t.Fatal("Clone must isolate the original")
	}
	// Closing something the snapshot never listed is a no-op.
	if r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 99, State: "closed"}) {
		t.Fatal("unlisted close must not report a change")
	}
	if got := r.TotalByRepo["org/repo"].Issues; got != 3 {
		t.Fatalf("unlisted close moved the count to %d", got)
	}
}

// A merged PR leaves PRs.Items and the held / stale-draft lists.
func TestApplyWebhookItem_MergedPRRemoved(t *testing.T) {
	r := patchFixture()
	r.PRs.Held = []PullRequest{{Repo: "org/repo", Number: 11}}
	r.Hold.Items = append(r.Hold.Items, HoldItem{Repo: "org/repo", Number: 11, Type: "pr"})
	r.recount()
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "closed", Merged: true})
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "Org/Repo", Number: 11, IsPR: true, State: "closed"})
	if r.PRs.Count != 0 || len(r.PRs.Held) != 0 || r.Hold.PRs != 0 || r.Hold.Total != 1 {
		t.Fatalf("after merge/close: prs=%+v hold=%+v", r.PRs, r.Hold)
	}
	if got := r.TotalByRepo["org/repo"].PRs; got != 0 {
		t.Fatalf("repo PR count = %d", got)
	}
}

// An open item the snapshot never listed is upserted into the bucket the
// classifier chose and counted as new open work.
func TestApplyWebhookItem_NewItemsUpserted(t *testing.T) {
	r := patchFixture()
	newIssue := &Issue{Repo: "org/repo", Number: 5, Title: "fresh", CreatedAt: time.Now()}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 5, State: "open", Title: "fresh",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: newIssue}}) {
		t.Fatal("expected upsert")
	}
	newPR := &PullRequest{Repo: "org/repo", Number: 12, Title: "new pr", State: "open"}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 12, IsPR: true, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, PR: newPR}})
	heldPR := &PullRequest{Repo: "org/repo", Number: 13, State: "open", Labels: []string{"hold"}}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 13, IsPR: true, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketHeld, PR: heldPR, Hold: &HoldItem{Repo: "org/repo", Number: 13, Type: "pr"}}})
	draft := &RepoWorkPR{Repo: "org/repo", Number: 14, Bucket: "draft"}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 14, IsPR: true, State: "open", Draft: true,
		Patch: WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkPR: draft}})
	// Unknown bucket: never listed, so nothing to patch — left for reconcile.
	if r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 15, State: "open"}) {
		t.Fatal("unknown bucket for an unlisted item must be a no-op")
	}

	if r.Issues.Count != 3 || r.PRs.Count != 2 || len(r.PRs.Held) != 1 || r.Hold.PRs != 1 || r.Hold.Total != 2 {
		t.Fatalf("after upserts: issues=%d prs=%d held=%d hold=%+v", r.Issues.Count, r.PRs.Count, len(r.PRs.Held), r.Hold)
	}
	if got := r.TotalByRepo["org/repo"]; got != (RepoCounts{Issues: 5, PRs: 4}) {
		t.Fatalf("repo counts = %+v", got)
	}
	if got := r.WorkDetailsByRepo["org/repo"].PRs; len(got) != 1 || got[0].Number != 14 {
		t.Fatalf("sidelined PRs = %+v", got)
	}
}

// A hold label added by a webhook moves the item from actionable to held;
// removed, it moves back. Unknown classification keeps the row where it
// was with its fields refreshed.
func TestApplyWebhookItem_BucketMovesAndUnknownPatchesInPlace(t *testing.T) {
	r := patchFixture()
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 1, State: "open", Title: "one (held)", Labels: []string{"hold"},
		Patch: WebhookItemPatch{Bucket: WebhookBucketHeld, Hold: &HoldItem{Repo: "org/repo", Number: 1, Type: "issue", Title: "one (held)", Labels: []string{"hold"}}}})
	if r.Issues.Count != 1 || r.Hold.Issues != 2 {
		t.Fatalf("after hold: issues=%d hold=%+v", r.Issues.Count, r.Hold)
	}
	if got := r.TotalByRepo["org/repo"].Issues; got != 4 {
		t.Fatalf("a move must not change the open count, got %d", got)
	}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 3, State: "open", Title: "released",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &Issue{Repo: "org/repo", Number: 3, Title: "released"}}})
	if r.Issues.Count != 2 || r.Hold.Issues != 1 || r.Hold.Items[0].Number != 1 {
		t.Fatalf("after release: issues=%+v hold=%+v", r.Issues.Items, r.Hold.Items)
	}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 2, State: "open", Title: "two renamed", Labels: []string{"kind/doc"}}) {
		t.Fatal("unknown bucket on a listed item should patch in place")
	}
	var two *Issue
	for i := range r.Issues.Items {
		if r.Issues.Items[i].Number == 2 {
			two = &r.Issues.Items[i]
		}
	}
	if two == nil || two.Title != "two renamed" || len(two.Labels) != 1 || two.Labels[0] != "kind/doc" {
		t.Fatalf("in-place patch: %+v", two)
	}
	if r.Issues.Count != 2 || r.TotalByRepo["org/repo"].Issues != 4 {
		t.Fatalf("in-place patch changed counts: %d / %d", r.Issues.Count, r.TotalByRepo["org/repo"].Issues)
	}
}

// A PR that moves bucket on a webhook keeps the closing references its
// previous row carried: the single REST read cannot see them, and the
// re-bucketed row must not render "no linked issue" until the next
// reconcile tick (hivecommons/hive#13448).
func TestApplyWebhookItem_RebucketedPRKeepsLinkedIssues(t *testing.T) {
	linked := []PRLinkedIssue{{Number: 1, Repo: "org/repo", Title: "one"}}
	r := patchFixture()
	r.PRs.Items[0].LinkedIssues = linked

	// Actionable -> draft (sidelined WorkPR row).
	draft := &RepoWorkPR{Repo: "org/repo", Number: 10, Bucket: "draft", Draft: true}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "open", Draft: true,
		Patch: WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkPR: draft}}) {
		t.Fatal("expected a move")
	}
	got := r.WorkDetailsByRepo["org/repo"].PRs
	if len(got) != 1 || len(got[0].LinkedIssues) != 1 || got[0].LinkedIssues[0].Number != 1 {
		t.Fatalf("sidelined row lost linked issues: %+v", got)
	}
	if draft.LinkedIssues != nil {
		t.Fatal("the caller's patch must not be mutated")
	}

	// Draft -> ready (actionable PullRequest row).
	ready := &PullRequest{Repo: "org/repo", Number: 10, State: "open"}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, PR: ready}})
	if r.PRs.Count != 1 || len(r.PRs.Items[0].LinkedIssues) != 1 || r.PRs.Items[0].LinkedIssues[0].Number != 1 {
		t.Fatalf("actionable row lost linked issues: %+v", r.PRs.Items)
	}

	// A read that already carries closing references wins over the old row.
	fresh := []PRLinkedIssue{{Number: 2, Repo: "org/repo"}}
	r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "open",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, PR: &PullRequest{Repo: "org/repo", Number: 10, State: "open", LinkedIssues: fresh}}})
	if li := r.PRs.Items[0].LinkedIssues; len(li) != 1 || li[0].Number != 2 {
		t.Fatalf("fresh linked issues overwritten: %+v", li)
	}
}

// FetchWebhookItemSnapshot classifies the single read for the governor:
// one GET, no list call, and the bucket follows the same label gates as
// the full enumeration.
func TestFetchWebhookItemSnapshot_ClassifiesForGovernor(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	// Earlier tests in the binary may leave the process-wide core budget in
	// its critical band or engage rate-limit caution. This test exercises the
	// reads, not either discretionary-read gate.
	resetRESTAccountingForTest(t, clock.now)
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/issues/21":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 21, "title": "Plain bug", "state": "open", "body": "steps",
				"user":   map[string]any{"login": "alice"},
				"labels": []map[string]any{{"name": "kind/bug"}}, "created_at": clock.now.Add(-time.Hour).Format(time.RFC3339),
			})
		case "/repos/org/repo/issues/22":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 22, "title": "Parked", "state": "open",
				"user":   map[string]any{"login": "alice"},
				"labels": []map[string]any{{"name": "hold"}},
			})
		case "/repos/org/repo/pulls/30":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 30, "title": "Ready PR", "state": "open", "draft": false,
				"user": map[string]any{"login": "bob"}, "head": map[string]any{"sha": "abc", "ref": "fix"},
				"base": map[string]any{"sha": "def", "ref": "main"}, "body": "x",
			})
		case "/repos/org/repo/pulls/31":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 31, "title": "WIP", "state": "open", "draft": true,
				"user": map[string]any{"login": "bob"}, "body": "x",
			})
		default:
			lists.Add(1)
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	ctx := context.Background()

	plain, err := c.FetchWebhookItemSnapshot(ctx, "repo", 21, false)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Patch.Bucket != WebhookBucketActionable || plain.Patch.Issue == nil || plain.Patch.Issue.Number != 21 || plain.Patch.Issue.Body != "steps" {
		t.Fatalf("plain issue patch = %+v", plain.Patch)
	}
	held, _ := c.FetchWebhookItemSnapshot(ctx, "repo", 22, false)
	if held.Patch.Bucket != WebhookBucketHeld || held.Patch.Hold == nil || held.Patch.Hold.Type != "issue" {
		t.Fatalf("held issue patch = %+v", held.Patch)
	}
	ready, _ := c.FetchWebhookItemSnapshot(ctx, "repo", 30, true)
	if ready.Patch.Bucket != WebhookBucketActionable || ready.Patch.PR == nil || ready.Patch.PR.HeadSHA != "abc" || ready.Patch.PR.BaseRef != "main" {
		t.Fatalf("ready PR patch = %+v", ready.Patch)
	}
	wip, _ := c.FetchWebhookItemSnapshot(ctx, "repo", 31, true)
	if wip.Patch.Bucket != WebhookBucketSidelined || wip.Patch.WorkPR == nil || wip.Patch.WorkPR.Bucket != workBucketPRDraft {
		t.Fatalf("draft PR patch = %+v", wip.Patch)
	}
	if lists.Load() != 0 {
		t.Fatalf("targeted re-reads must not list: %d unexpected calls", lists.Load())
	}

	// Applying all four to a snapshot yields the governor's view without a list.
	r := &ActionableResult{TotalByRepo: map[string]RepoCounts{"repo": {}}}
	for _, s := range []WebhookItemSnapshot{plain, held, ready, wip} {
		r.ApplyWebhookItem(s)
	}
	if r.Issues.Count != 1 || r.PRs.Count != 1 || r.Hold.Total != 1 || len(r.WorkDetailsByRepo["repo"].PRs) != 1 {
		t.Fatalf("assembled snapshot: issues=%d prs=%d hold=%d details=%+v", r.Issues.Count, r.PRs.Count, r.Hold.Total, r.WorkDetailsByRepo)
	}
	if got := r.TotalByRepo["repo"]; got != (RepoCounts{Issues: 2, PRs: 2}) {
		t.Fatalf("repo counts = %+v", got)
	}
}

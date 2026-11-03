package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A re-read whose updated_at is older than the copy already held (two
// deliveries' GETs landing out of order) is dropped; a newer one lands and
// records the delivery id, fetch time and head SHA for audit.
func TestApplyWebhookItem_MonotonicUpsertRecordsDelivery(t *testing.T) {
	now := time.Now()
	r := patchFixture()
	r.Issues.Items[0].UpdatedAt = now.Add(-time.Minute)
	r.Issues.Count = len(r.Issues.Items)

	fresh := Issue{Repo: "org/repo", Number: 1, Title: "one (edited)", UpdatedAt: now}
	newer := WebhookItemSnapshot{
		Repo: "org/repo", Number: 1, State: "open", Title: fresh.Title, UpdatedAt: now,
		FetchedAt: now, DeliveryID: "d-2",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &fresh},
	}
	if !r.ApplyWebhookItem(newer) {
		t.Fatal("newer read must apply")
	}
	meta, ok := r.ItemPatches[ItemPatchKey("org/repo", 1, false)]
	if !ok || meta.DeliveryID != "d-2" || !meta.FetchedAt.Equal(now) || !meta.UpdatedAt.Equal(now) {
		t.Fatalf("patch meta = %+v ok=%v", meta, ok)
	}
	if got := r.ItemFetchedAt("org/repo", 1, false); !got.Equal(now) {
		t.Fatalf("ItemFetchedAt = %v, want the patch time", got)
	}
	if got := r.ItemFetchedAt("org/repo", 2, false); !got.Equal(r.GeneratedAt) {
		t.Fatalf("unpatched item must report the enumeration time, got %v", got)
	}

	stale := Issue{Repo: "org/repo", Number: 1, Title: "one (old)", UpdatedAt: now.Add(-30 * time.Second)}
	older := WebhookItemSnapshot{
		Repo: "org/repo", Number: 1, State: "open", Title: stale.Title, UpdatedAt: stale.UpdatedAt,
		FetchedAt: now.Add(time.Second), DeliveryID: "d-1",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &stale},
	}
	if r.ApplyWebhookItem(older) {
		t.Fatal("older read must be dropped")
	}
	if got := issueByNumber(r, 1); got == nil || got.Title != "one (edited)" || r.ItemPatches[ItemPatchKey("org/repo", 1, false)].DeliveryID != "d-2" {
		t.Fatalf("older read moved the snapshot backwards: %+v", got)
	}
	// An older read may not even resurrect a removed item.
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 1, State: "closed", UpdatedAt: now.Add(time.Minute), DeliveryID: "d-3"}) {
		t.Fatal("close must apply")
	}
	if r.ApplyWebhookItem(newer) || r.Issues.Count != 1 {
		t.Fatalf("late open read resurrected a closed issue: %+v", r.Issues)
	}
	if r.ItemPatches[ItemPatchKey("org/repo", 1, false)].DeliveryID != "d-3" {
		t.Fatal("removal must leave its delivery id for audit")
	}

	// PRs: head SHA rides along.
	pr := PullRequest{Repo: "org/repo", Number: 10, State: "open", HeadSHA: "new", UpdatedAt: now}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "open", UpdatedAt: now, HeadSHA: "new", DeliveryID: "d-4",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, PR: &pr}}) {
		t.Fatal("PR read must apply")
	}
	if r.ItemPatches[ItemPatchKey("org/repo", 10, true)].HeadSHA != "new" {
		t.Fatal("head SHA not recorded")
	}
	// Clone carries the audit map independently.
	c := r.Clone()
	c.ItemPatches["x"] = ItemPatchMeta{}
	if _, leaked := r.ItemPatches["x"]; leaked {
		t.Fatal("Clone shared ItemPatches")
	}
}

// closed removes at once; a later reopened read (newer updated_at) re-adds.
func TestApplyWebhookItem_ReopenedReAdds(t *testing.T) {
	now := time.Now()
	r := patchFixture()
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 2, State: "closed", UpdatedAt: now}) {
		t.Fatal("close must apply")
	}
	if r.Issues.Count != 1 || r.TotalByRepo["org/repo"].Issues != 3 {
		t.Fatalf("after close: %+v", r.Issues)
	}
	back := Issue{Repo: "org/repo", Number: 2, Title: "two", UpdatedAt: now.Add(time.Minute)}
	if !r.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 2, State: "open", UpdatedAt: back.UpdatedAt,
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &back}}) {
		t.Fatal("reopen must apply")
	}
	if r.Issues.Count != 2 || r.TotalByRepo["org/repo"].Issues != 4 {
		t.Fatalf("after reopen: issues=%d counts=%+v", r.Issues.Count, r.TotalByRepo)
	}
}

// MarkAllStale and the enumerator's last-good path both leave a
// stale-since marker: the time the figures were last read for real.
func TestActionableResult_StaleSince(t *testing.T) {
	gen := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	r := &ActionableResult{GeneratedAt: gen, TotalByRepo: map[string]RepoCounts{"org/a": {}, "org/b": {}}}
	r.MarkAllStale()
	if !r.Stale() || !r.StaleSince.Equal(gen) || !r.RepoStale("ORG/A") || r.RepoStale("org/c") {
		t.Fatalf("MarkAllStale: stale=%v since=%v repos=%v", r.Stale(), r.StaleSince, r.StaleRepos)
	}
	r.GeneratedAt = gen.Add(time.Hour)
	r.MarkAllStale()
	if !r.StaleSince.Equal(gen) {
		t.Fatal("a second failure must keep the original stale-since")
	}
}

// The irreversible-action guard: fresh copy → no call; old copy → one GET
// and proceed only if still open; stale repo → refused without a call.
func TestSnapshotFreshnessGuard(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	resetRESTAccountingForTest(t, clock.now)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		switch r.URL.Path {
		case "/repos/org/repo/issues/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "state": "open", "user": map[string]any{"login": "a"}})
		case "/repos/org/repo/issues/2":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "state": "closed", "user": map[string]any{"login": "a"}})
		case "/repos/org/repo/pulls/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 10, "state": "closed", "merged": true, "user": map[string]any{"login": "a"}})
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	ctx := context.Background()

	snap := &ActionableResult{GeneratedAt: clock.now.Add(-DecisionFreshnessBound / 2), TotalByRepo: map[string]RepoCounts{"org/repo": {Issues: 2}}}
	guard := c.SnapshotFreshnessGuard(snap, clock.Now)
	if err := guard(ctx, "org/repo", 1, false); err != nil || gets.Load() != 0 {
		t.Fatalf("fresh copy: err=%v gets=%d", err, gets.Load())
	}

	snap.GeneratedAt = clock.now.Add(-2 * DecisionFreshnessBound)
	if err := guard(ctx, "org/repo", 1, false); err != nil || gets.Load() != 1 {
		t.Fatalf("old open copy: err=%v gets=%d, want one confirming GET", err, gets.Load())
	}
	if err := guard(ctx, "org/repo", 2, false); !errors.Is(err, ErrItemNotOpen) {
		t.Fatalf("old closed copy: err=%v, want ErrItemNotOpen", err)
	}
	if err := guard(ctx, "org/repo", 10, true); !errors.Is(err, ErrItemNotOpen) {
		t.Fatalf("merged PR: err=%v, want ErrItemNotOpen", err)
	}
	// A webhook patch newer than the bound counts as fresh.
	snap.ItemPatches = map[string]ItemPatchMeta{ItemPatchKey("org/repo", 1, false): {FetchedAt: clock.now}}
	before := gets.Load()
	if err := guard(ctx, "org/repo", 1, false); err != nil || gets.Load() != before {
		t.Fatalf("patched copy: err=%v gets=%d", err, gets.Load()-before)
	}

	snap.StaleRepos = []string{"org/repo"}
	if err := guard(ctx, "org/repo", 1, false); !errors.Is(err, ErrSnapshotStaleForDecision) || gets.Load() != before {
		t.Fatalf("stale repo: err=%v gets=%d, want refusal without a call", err, gets.Load()-before)
	}

	// CloseIssue and MergePR consult the guard from ctx before any write.
	gctx := WithIrreversibleGuard(ctx, guard)
	if err := c.CloseIssue(gctx, "repo", 1, IssueCloseOptions{}); !errors.Is(err, ErrSnapshotStaleForDecision) {
		t.Fatalf("CloseIssue under stale guard: %v", err)
	}
	if _, err := c.MergePR(gctx, "repo", 10, "squash", ""); !errors.Is(err, ErrSnapshotStaleForDecision) {
		t.Fatalf("MergePR under stale guard: %v", err)
	}
	if gets.Load() != before {
		t.Fatalf("refused actions must not call GitHub: %d calls", gets.Load()-before)
	}
	if CheckIrreversible(ctx, "org/repo", 1, false) != nil || c.SnapshotFreshnessGuard(nil, nil) != nil {
		t.Fatal("no guard / no snapshot must not gate")
	}
}

func issueByNumber(r *ActionableResult, number int) *Issue {
	for i := range r.Issues.Items {
		if r.Issues.Items[i].Number == number {
			return &r.Issues.Items[i]
		}
	}
	return nil
}

package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ParseBlockedOnReason reads back what blockedFilterReason wrote, and
// BlockedOnTarget prefers the body marker over the reason.
func TestParseBlockedOnReason(t *testing.T) {
	body := "Spun off from the review backlog.\n\n**Blocked on PR #14862.**\n" + BlockedOnMarker("hivecommons/hive", 14862)
	reason := blockedFilterReason(body)
	repo, pr, ok := ParseBlockedOnReason(reason)
	if !ok || repo != "hivecommons/hive" || pr != 14862 {
		t.Fatalf("ParseBlockedOnReason(%q) = %q %d %v", reason, repo, pr, ok)
	}
	for _, bad := range []string{"", "blocked", "blocked until 2026-10-28T13:00:00Z", "Exempt label", "blocked on #14862"} {
		if _, _, ok := ParseBlockedOnReason(bad); ok {
			t.Fatalf("ParseBlockedOnReason(%q) should not parse", bad)
		}
	}
	if repo, pr, ok := BlockedOnTarget("", reason); !ok || repo != "hivecommons/hive" || pr != 14862 {
		t.Fatalf("BlockedOnTarget from reason = %q %d %v", repo, pr, ok)
	}
	if repo, pr, ok := BlockedOnTarget(BlockedOnMarker("acme/api", 7), reason); !ok || repo != "acme/api" || pr != 7 {
		t.Fatalf("BlockedOnTarget should prefer the body marker, got %q %d %v", repo, pr, ok)
	}
	if _, _, ok := BlockedOnTarget("no marker", "blocked"); ok {
		t.Fatal("BlockedOnTarget without marker or reason should not parse")
	}
}

// unblockIssue removes the label and tells the ItemRefreshHook, so the
// hive's own unblock re-reads the row the way an inbound webhook does; a
// failed removal does not fire it.
func TestUnblockIssue_FiresItemRefreshHook(t *testing.T) {
	type call struct {
		repo   string
		number int
		isPR   bool
	}
	var calls []call
	SetItemRefreshHook(func(repo string, number int, isPR bool) { calls = append(calls, call{repo, number, isPR}) })
	t.Cleanup(func() { SetItemRefreshHook(nil) })

	srv, c := newLabelMutationServer(t, "agent/reviewer", BlockedLabel)
	if err := c.unblockIssue(context.Background(), "acme/widgets", 7); err != nil {
		t.Fatalf("unblockIssue: %v", err)
	}
	srv.mu.Lock()
	dels, labels := srv.dels, append([]string(nil), srv.labels...)
	srv.mu.Unlock()
	if dels != 1 || labelsAllPresent(labels, []string{BlockedLabel}) {
		t.Fatalf("dels=%d labels=%v, want the blocked label removed", dels, labels)
	}
	if len(calls) != 1 || calls[0] != (call{"acme/widgets", 7, false}) {
		t.Fatalf("hook calls = %+v, want one for acme/widgets#7", calls)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/8/labels/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	bad := httptest.NewServer(mux)
	t.Cleanup(bad.Close)
	c2 := NewClientForTest(bad.URL, "acme", []string{"widgets"}, testLogger())
	if err := c2.unblockIssue(context.Background(), "acme/widgets", 8); err == nil {
		t.Fatal("unblockIssue should surface the removal failure")
	}
	if len(calls) != 1 {
		t.Fatalf("a failed removal must not fire the hook, calls = %+v", calls)
	}
}

// A reconcile pass that began before a webhook re-read lands publishes the
// re-read, not its own older copy of the item: the hive's own `blocked`
// removal, read within seconds, must not be rolled back by the listing
// already in flight (hivecommons/hive#14863).
func TestReplayWebhookPatches_NewerReadWinsOverInFlightListing(t *testing.T) {
	listedAt := time.Date(2026, 10, 28, 6, 5, 0, 0, time.UTC)
	started := time.Date(2026, 10, 28, 12, 38, 0, 0, time.UTC)
	unblockedAt := time.Date(2026, 10, 28, 12, 41, 28, 0, time.UTC)
	fresh := &ActionableResult{
		GeneratedAt: started,
		Issues:      IssueResultFromItems([]Issue{{Repo: "org/repo", Number: 1, Title: "one", UpdatedAt: listedAt}}),
		TotalByRepo: map[string]RepoCounts{"org/repo": {Issues: 3}},
		WorkDetailsByRepo: map[string]RepoWorkDetails{"org/repo": {Issues: []RepoWorkIssue{
			{Repo: "org/repo", Number: 14863, Title: "Review backlog", Labels: []string{"agent/reviewer", BlockedLabel}, UpdatedAt: listedAt, Bucket: "filtered", Reason: "blocked on org/repo#14862"},
			{Repo: "org/repo", Number: 14868, Title: "Still blocked", Labels: []string{BlockedLabel}, UpdatedAt: listedAt, Bucket: "filtered", Reason: "blocked on org/repo#14821"},
		}}},
	}

	prev := fresh.Clone()
	prev.GeneratedAt = started.Add(-time.Hour)
	// Re-read after the listing started: the label is gone, the row is actionable.
	if !prev.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 14863, State: "open", Title: "Review backlog", Labels: []string{"agent/reviewer"},
		UpdatedAt: unblockedAt, FetchedAt: unblockedAt.Add(2 * time.Second), DeliveryID: "self:edit",
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &Issue{Repo: "org/repo", Number: 14863, Title: "Review backlog", Labels: []string{"agent/reviewer"}, UpdatedAt: unblockedAt}}}) {
		t.Fatal("unblock re-read should patch the previous snapshot")
	}
	// Re-read before the listing started: the listing is newer, must not replay.
	if !prev.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 14868, State: "open", Title: "Renamed early", Labels: []string{BlockedLabel},
		UpdatedAt: started.Add(-30 * time.Minute), FetchedAt: started.Add(-29 * time.Minute),
		Patch: WebhookItemPatch{Bucket: WebhookBucketSidelined, WorkIssue: &RepoWorkIssue{Repo: "org/repo", Number: 14868, Title: "Renamed early", Labels: []string{BlockedLabel}, Bucket: "filtered", Reason: "blocked"}}}) {
		t.Fatal("early re-read should patch the previous snapshot")
	}
	// A read newer than the listing for an item the listing no longer has
	// (closed meanwhile): the listing wins, nothing to replay.
	prev.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 999, State: "open", Title: "gone",
		UpdatedAt: unblockedAt, FetchedAt: unblockedAt,
		Patch: WebhookItemPatch{Bucket: WebhookBucketActionable, Issue: &Issue{Repo: "org/repo", Number: 999, Title: "gone", UpdatedAt: unblockedAt}}})
	if len(prev.WebhookPatches) != 3 {
		t.Fatalf("WebhookPatches remembered %d, want 3", len(prev.WebhookPatches))
	}

	if n := fresh.ReplayWebhookPatches(prev); n != 1 {
		t.Fatalf("ReplayWebhookPatches = %d, want 1 (only the post-listing unblock)", n)
	}
	var found *Issue
	for i := range fresh.Issues.Items {
		if fresh.Issues.Items[i].Number == 14863 {
			found = &fresh.Issues.Items[i]
		}
	}
	if found == nil || len(found.Labels) != 1 || found.Labels[0] != "agent/reviewer" {
		t.Fatalf("14863 after replay = %+v, want actionable without the blocked label", found)
	}
	for _, w := range fresh.WorkDetailsByRepo["org/repo"].Issues {
		if w.Number == 14863 {
			t.Fatalf("14863 still sidelined after replay: %+v", w)
		}
		if w.Number == 14868 && w.Title != "Still blocked" {
			t.Fatalf("pre-listing read replayed onto 14868: %+v", w)
		}
	}
	for _, it := range fresh.Issues.Items {
		if it.Number == 999 {
			t.Fatal("an item the listing dropped must not be resurrected by an older read")
		}
	}
	if meta, ok := fresh.ItemPatches[ItemPatchKey("org/repo", 14863, false)]; !ok || meta.DeliveryID != "self:edit" {
		t.Fatalf("replayed read should be recorded in ItemPatches, got %+v %v", meta, ok)
	}
	if _, ok := fresh.WebhookPatches[ItemPatchKey("org/repo", 14863, false)]; !ok {
		t.Fatal("replayed read should be remembered for the next pass")
	}
	// Idempotent: a retry (compare-and-swap lost) changes nothing more.
	if n := fresh.ReplayWebhookPatches(prev); n != 0 {
		t.Fatalf("second replay = %d, want 0", n)
	}
	if fresh.ReplayWebhookPatches(nil) != 0 || fresh.ReplayWebhookPatches(fresh) != 0 {
		t.Fatal("nil / self replay must be a no-op")
	}
}

// A closed re-read newer than the listing removes the item the listing
// still carries; the memory is bounded and survives Clone.
func TestReplayWebhookPatches_ClosedReadAndBoundedMemory(t *testing.T) {
	started := time.Now().Add(-5 * time.Minute)
	fresh := &ActionableResult{
		GeneratedAt: started,
		PRs:         PRResult{Count: 1, Items: []PullRequest{{Repo: "org/repo", Number: 10, State: "open", UpdatedAt: started.Add(-time.Hour)}}},
		TotalByRepo: map[string]RepoCounts{"org/repo": {PRs: 1}},
	}
	prev := fresh.Clone()
	prev.ApplyWebhookItem(WebhookItemSnapshot{Repo: "org/repo", Number: 10, IsPR: true, State: "closed", Merged: true, UpdatedAt: started.Add(time.Minute), FetchedAt: started.Add(time.Minute)})
	cloned := prev.Clone()
	if len(cloned.WebhookPatches) != 1 {
		t.Fatalf("Clone dropped WebhookPatches: %d", len(cloned.WebhookPatches))
	}
	if n := fresh.ReplayWebhookPatches(cloned); n != 1 || fresh.PRs.Count != 0 || fresh.TotalByRepo["org/repo"].PRs != 0 {
		t.Fatalf("merged re-read not replayed: n=%d prs=%+v counts=%+v", n, fresh.PRs, fresh.TotalByRepo)
	}

	r := &ActionableResult{}
	for i := 1; i <= maxRememberedWebhookPatches+5; i++ {
		r.recordItemPatch(WebhookItemSnapshot{Repo: "org/repo", Number: i, FetchedAt: started.Add(time.Duration(i) * time.Second)})
	}
	if len(r.WebhookPatches) != maxRememberedWebhookPatches {
		t.Fatalf("memory = %d, want %d", len(r.WebhookPatches), maxRememberedWebhookPatches)
	}
	if _, ok := r.WebhookPatches[ItemPatchKey("org/repo", 1, false)]; ok {
		t.Fatal("oldest read should have been evicted first")
	}
	if _, ok := r.WebhookPatches[ItemPatchKey("org/repo", maxRememberedWebhookPatches+5, false)]; !ok {
		t.Fatal("newest read should be kept")
	}
}

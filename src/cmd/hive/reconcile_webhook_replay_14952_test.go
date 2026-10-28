package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// blockedIssueSnapshot is a reconcile pass's copy of org/repo#1 still
// parked as blocked, listed at listedAt.
func blockedIssueSnapshot(listedAt time.Time) *github.ActionableResult {
	return &github.ActionableResult{
		GeneratedAt: listedAt,
		TotalByRepo: map[string]github.RepoCounts{"org/repo": {Issues: 1}},
		WorkDetailsByRepo: map[string]github.RepoWorkDetails{"org/repo": {Issues: []github.RepoWorkIssue{{
			Repo: "org/repo", Number: 1, Title: "finding", Labels: []string{github.BlockedLabel},
			Bucket: "filtered", Reason: "blocked on org/repo#9", UpdatedAt: listedAt.Add(-6 * time.Hour),
		}}}},
	}
}

// unblockRead is the webhook re-read after the hive removed `blocked`.
func unblockRead(updatedAt, fetchedAt time.Time) github.WebhookItemSnapshot {
	return github.WebhookItemSnapshot{
		Repo: "org/repo", Number: 1, State: "open", UpdatedAt: updatedAt, FetchedAt: fetchedAt,
		Patch: github.WebhookItemPatch{Bucket: github.WebhookBucketActionable, Issue: &github.Issue{Repo: "org/repo", Number: 1, Title: "finding", UpdatedAt: updatedAt}},
	}
}

func issueOneListedActionable(r *github.ActionableResult) bool {
	for _, it := range r.Issues.Items {
		if it.Repo == "org/repo" && it.Number == 1 {
			return true
		}
	}
	return false
}

func issueOneSidelinedBlocked(r *github.ActionableResult) bool {
	for _, it := range r.WorkDetailsByRepo["org/repo"].Issues {
		if it.Number == 1 {
			return true
		}
	}
	return false
}

// hivecommons/hive#14952: a reconcile pass that listed the issue before the
// hive's own unblock and finished after the webhook re-read patched the
// snapshot must not publish its pre-unblock copy over the patch.
func TestReconcilePassReplaysWebhookReadsFetchedAfterItStarted(t *testing.T) {
	now := time.Now()
	passStart := now.Add(-4 * time.Minute)

	var last atomic.Pointer[github.ActionableResult]
	last.Store(blockedIssueSnapshot(now.Add(-30 * time.Minute)))
	p := &snapshotPatcher{target: &last}
	if p.apply([]github.WebhookItemSnapshot{unblockRead(now.Add(-time.Minute), now.Add(-time.Minute))}) != 1 {
		t.Fatal("unblock re-read not applied")
	}
	if !issueOneListedActionable(last.Load()) || len(last.Load().WebhookReads) != 1 {
		t.Fatalf("patched snapshot does not carry the unblock: %+v", last.Load().Issues)
	}

	// The pass that started before the unblock now publishes its stale copy.
	stale := blockedIssueSnapshot(passStart)
	publishReconciledSnapshot(&last, stale, passStart)

	got := last.Load()
	if !issueOneListedActionable(got) || issueOneSidelinedBlocked(got) {
		t.Fatalf("reconcile pass overwrote the webhook unblock: issues=%+v work=%+v", got.Issues, got.WorkDetailsByRepo)
	}
	if len(got.WebhookReads) != 1 {
		t.Fatalf("replayed read not kept for the next pass: %d", len(got.WebhookReads))
	}
}

// A read fetched before the pass started is already covered by the pass's
// own listing, which stays authoritative.
func TestReconcilePassDoesNotReplayReadsOlderThanItsStart(t *testing.T) {
	now := time.Now()
	var last atomic.Pointer[github.ActionableResult]
	last.Store(blockedIssueSnapshot(now.Add(-time.Hour)))
	p := &snapshotPatcher{target: &last}
	p.apply([]github.WebhookItemSnapshot{unblockRead(now.Add(-20*time.Minute), now.Add(-20*time.Minute))})

	passStart := now.Add(-10 * time.Minute)
	fresh := blockedIssueSnapshot(passStart)
	publishReconciledSnapshot(&last, fresh, passStart)

	got := last.Load()
	if got != fresh || !issueOneSidelinedBlocked(got) || issueOneListedActionable(got) {
		t.Fatalf("pre-pass read replayed onto the reconcile result: %+v", got.Issues)
	}
	if len(got.WebhookReads) != 0 {
		t.Fatalf("pre-pass reads must be dropped: %d", len(got.WebhookReads))
	}
}

// A read whose updated_at is older than the pass's own copy is dropped by
// the monotonic guard even when fetched after the pass started.
func TestReconcilePassReplayKeepsNewerListedCopy(t *testing.T) {
	now := time.Now()
	passStart := now.Add(-4 * time.Minute)
	var last atomic.Pointer[github.ActionableResult]
	last.Store(&github.ActionableResult{GeneratedAt: now.Add(-time.Hour), TotalByRepo: map[string]github.RepoCounts{"org/repo": {}}})
	p := &snapshotPatcher{target: &last}
	if p.apply([]github.WebhookItemSnapshot{unblockRead(now.Add(-8*time.Hour), now.Add(-time.Minute))}) != 1 {
		t.Fatal("read not applied to a snapshot that never listed the item")
	}

	fresh := blockedIssueSnapshot(passStart)
	publishReconciledSnapshot(&last, fresh, passStart)
	if got := last.Load(); !issueOneSidelinedBlocked(got) || issueOneListedActionable(got) {
		t.Fatalf("older read replaced the pass's newer copy: %+v", got.Issues)
	}
}

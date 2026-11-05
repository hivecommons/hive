package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestStageTrackerRecentlyMergedAnyMerger (hivecommons/hive#16658):
// recently_merged is the union of the audit's pr_merged lines and the
// merged ledger, each naming the merger — so a PR a maintainer merged by
// hand (`gh pr merge --admin`), which reaches the ledger through the
// webhook's closed read long before the PR poller writes an audit line,
// is a terminal fact for the SPA the moment its row leaves. One merge
// known to both sources is listed once, newest first.
func TestStageTrackerRecentlyMergedAnyMerger(t *testing.T) {
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	ts := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }

	s.audit.LogRecordAt(ts(3*time.Minute), "system", github.AuditActionPRMerged, "method=squash, path=sweep", "governor", "acme/console", 16645)
	s.recordMergedSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 16644, IsPR: true, State: "closed", Merged: true, MergedAt: now.Add(-time.Minute), MergedBy: "clubanderson"},
		{Repo: "acme/console", Number: 16645, IsPR: true, State: "closed", Merged: true, MergedAt: now.Add(-3 * time.Minute), MergedBy: "hive-bot", MergedByHive: true},
		{Repo: "acme/console", Number: 16600, IsPR: true, State: "closed", Merged: true, MergedAt: now.Add(-stageTrackerMergedWindow - time.Minute), MergedBy: "alice"},
		{Repo: "acme/console", Number: 16646, IsPR: true, State: "closed", Merged: false, UpdatedAt: now.Add(-time.Minute)},
	})

	got := s.buildStageTracker(s.status, store, now).RecentlyMerged
	want := []StageTrackerMerged{
		{Repo: "acme/console", Number: 16644, MergedAt: now.Add(-time.Minute), MergedBy: "clubanderson"},
		{Repo: "acme/console", Number: 16645, MergedAt: now.Add(-3 * time.Minute), MergedBy: MergedByHive},
	}
	if len(got) != len(want) {
		t.Fatalf("recently_merged = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Repo != want[i].Repo || got[i].Number != want[i].Number || !got[i].MergedAt.Equal(want[i].MergedAt) || got[i].MergedBy != want[i].MergedBy {
			t.Fatalf("recently_merged[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestStageTrackerClosedSnapshotNeverOutranksMerge (hivecommons/hive#16658):
// a closed-but-not-merged snapshot for a PR the ledger holds as merged
// (an issue_comment hint cannot tell the two apart) is not listed in
// recently_closed, and a later merged snapshot for a PR already listed
// there withdraws it — "closed" is never the SPA's final word on a merge.
func TestStageTrackerClosedSnapshotNeverOutranksMerge(t *testing.T) {
	s := &Server{}
	now := time.Now()
	closed := func(n int) github.WebhookItemSnapshot {
		return github.WebhookItemSnapshot{Repo: "o/r", Number: n, IsPR: true, State: "closed", UpdatedAt: now}
	}
	merged := func(n int, by string) github.WebhookItemSnapshot {
		return github.WebhookItemSnapshot{Repo: "o/r", Number: n, IsPR: true, State: "closed", Merged: true, MergedAt: now, MergedBy: by}
	}
	// Merge known first: the closed read is ignored.
	s.recordMergedSnapshots([]github.WebhookItemSnapshot{merged(16644, "clubanderson")})
	s.noteClosedSnapshots([]github.WebhookItemSnapshot{closed(16644), closed(16646)})
	if got := s.stageTrackerRecentlyClosed(now); len(got) != 1 || got[0].Number != 16646 {
		t.Fatalf("recently closed = %+v, want [16646]", got)
	}
	// Closed read first, merge after: the entry is withdrawn.
	s.noteClosedSnapshots([]github.WebhookItemSnapshot{closed(16647)})
	if got := s.stageTrackerRecentlyClosed(now); len(got) != 2 {
		t.Fatalf("recently closed = %+v, want 16647 and 16646", got)
	}
	snaps := []github.WebhookItemSnapshot{merged(16647, "clubanderson")}
	s.recordMergedSnapshots(snaps)
	s.forgetClosedSnapshots(snaps)
	if got := s.stageTrackerRecentlyClosed(now); len(got) != 1 || got[0].Number != 16646 {
		t.Fatalf("recently closed after the merge = %+v, want [16646]", got)
	}
	if !s.mergedLedger().Has("o/r", 16647) || s.mergedLedger().Has("o/r", 16646) {
		t.Fatal("ledger: want 16647 merged and 16646 not")
	}
}

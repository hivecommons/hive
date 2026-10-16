package evidence

import (
	"testing"
	"time"
)

// TestFoldPRsLiveAndBackfilled pins the per-PR fold every evidence reader
// shares (hivecommons/hive#12516): a live bundle written when review started
// and a backfilled bundle stamped with the PR's open time and forge outcome
// are one PR with the live verdicts, the backfilled open time, merge and
// links, and a backfilled close without merge is Closed unless any record of
// the PR knows a merge.
func TestFoldPRsLiveAndBackfilled(t *testing.T) {
	opened := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	reviewed := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	mk := func(repo string, n int, head string, created time.Time) *Bundle {
		return &Bundle{
			SchemaVersion: SchemaVersion, ID: BundleID(repo, n, head), Repo: repo, Number: n, HeadSHA: head,
			Author: Author{Login: "bee", Kind: AuthorAgent}, CreatedAt: created, UpdatedAt: created,
		}
	}
	backfilled := func(b *Bundle, state string) *Bundle {
		b.Backfilled = true
		b.Backfill = &Backfill{Source: "github-api", At: reviewed.Add(time.Hour), State: state}
		return b
	}

	// PR 1: backfilled merge + revert link, then a live review of a newer head.
	bf1 := backfilled(mk("O/R", 1, "aaa", opened), BackfillStateMerged)
	bf1.Merge = &MergeEvent{At: opened.Add(48 * time.Hour), OpenedAt: opened}
	bf1.RevertedBy = []string{"o/r#9@x"}
	bf1.FollowupOf = []string{"o/r#0@y"}
	live1 := mk("o/r", 1, "bbb", reviewed)
	live1.Verdicts = []Verdict{{Perspective: "security", Verdict: "approve"}}

	// PR 2: backfilled close without merge, plus a live record.
	bf2 := backfilled(mk("o/r", 2, "ccc", opened), BackfillStateClosed)
	live2 := mk("o/r", 2, "ddd", reviewed)

	// PR 3: an older backfill said closed, a later record knows the merge.
	bf3 := backfilled(mk("o/r", 3, "eee", opened), BackfillStateClosed)
	merged3 := mk("o/r", 3, "fff", reviewed)
	merged3.Merge = &MergeEvent{At: reviewed.Add(time.Hour)}

	// PR 4: same created_at; the later updated_at wins.
	a4, b4 := mk("o/r", 4, "ggg", reviewed), mk("o/r", 4, "hhh", reviewed)
	b4.UpdatedAt = reviewed.Add(time.Minute)

	// Other repo and kind, for the filters.
	other := mk("x/y", 5, "iii", reviewed)
	other.Author.Kind = AuthorHuman

	prs := FoldPRs([]*Bundle{live1, nil, bf1, bf2, live2, bf3, merged3, b4, a4, other}, "", "")
	if len(prs) != 5 {
		t.Fatalf("got %d PRs, want 5: %v", len(prs), prs)
	}

	p1 := prs[PRKey("o/r", 1)]
	if p1 == nil || p1.Bundle != live1 {
		t.Fatalf("PR 1 must take its verdicts from the newest (live) bundle: %+v", p1)
	}
	if !p1.OpenedAt.Equal(opened) || p1.Merge != bf1.Merge || p1.Closed {
		t.Errorf("PR 1 open time / merge must come from the backfilled bundle: opened=%v merge=%v closed=%v", p1.OpenedAt, p1.Merge, p1.Closed)
	}
	if len(p1.RevertedBy) != 1 || len(p1.FollowupOf) != 1 {
		t.Errorf("PR 1 links must come from whichever bundle has them: reverted_by=%v followup_of=%v", p1.RevertedBy, p1.FollowupOf)
	}

	if p2 := prs[PRKey("o/r", 2)]; p2.Bundle != live2 || !p2.Closed || p2.Merge != nil || !p2.OpenedAt.Equal(opened) {
		t.Errorf("PR 2 must be closed with the backfilled open time: %+v", p2)
	}
	if p3 := prs[PRKey("o/r", 3)]; p3.Closed || p3.Merge != merged3.Merge {
		t.Errorf("PR 3: a merge on any record clears a backfilled close: %+v", p3)
	}
	if p4 := prs[PRKey("o/r", 4)]; p4.Bundle != b4 {
		t.Errorf("PR 4: equal created_at must fall back to updated_at, got head %s", p4.HeadSHA)
	}

	// A merge.opened_at earlier than created_at is the open time even when
	// it is the only record.
	solo := mk("o/r", 6, "jjj", reviewed)
	solo.Merge = &MergeEvent{At: reviewed, OpenedAt: opened}
	if p := FoldPRs([]*Bundle{solo}, "", "")[PRKey("o/r", 6)]; !p.OpenedAt.Equal(opened) {
		t.Errorf("open time = %v, want merge.opened_at %v", p.OpenedAt, opened)
	}

	if got := FoldPRs([]*Bundle{live1, bf1, other}, "o/R", ""); len(got) != 1 || got[PRKey("o/r", 1)] == nil {
		t.Errorf("repo filter must be case-insensitive and keep both bundles of PR 1: %v", got)
	}
	if got := FoldPRs([]*Bundle{live1, bf1, other}, "", AuthorHuman); len(got) != 1 || got[PRKey("x/y", 5)] == nil {
		t.Errorf("author kind filter = %v", got)
	}
}

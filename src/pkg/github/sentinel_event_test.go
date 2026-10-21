package github

// Tests for the event-driven sentinel (#13379): SweepSentinelPR reads one
// PR and its files only; the reconcile cadence enumerates from the
// forgestate cache without listing and skips every head it already saw;
// without a cache the sweep lists as before.

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/forgestate"
)

type fakeSentinelPRCache struct {
	snaps map[string]forgestate.Snapshot
	calls int
}

func (f *fakeSentinelPRCache) Snapshot(repo string) (forgestate.Snapshot, bool) {
	f.calls++
	s, ok := f.snaps[repo]
	return s, ok
}

func ownersFile() []map[string]any {
	return []map[string]any{{
		"filename": "OWNERS", "status": "modified", "additions": 1, "deletions": 0,
		"patch": "@@ -5,3 +5,4 @@\n reviewers:\n   - clubanderson\n+  - vjymisal0\n",
	}}
}

func cachedPR(n int, head string, labels ...string) forgestate.Item {
	return forgestate.Item{Kind: forgestate.KindPR, Number: n, State: "open", HeadSHA: head, Labels: labels}
}

func TestSweepSentinelPREventReadsOnlyThatPR(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{
			sentinelPR(7, "vjymisal0", "aaa111"),
			sentinelPR(8, "mallory", "bbb222"),
		},
		files: map[int][]map[string]any{7: ownersFile(), 8: ownersFile()},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	var audited []SentinelSweepEvent
	res, err := c.SweepSentinelPR(context.Background(), "o/r", 7, defaultSentinelOpts(func(e SentinelSweepEvent) { audited = append(audited, e) }))
	if err != nil {
		t.Fatalf("event sweep: %v", err)
	}
	if res.Seen != 1 || len(res.Flagged) != 1 || len(res.Errors) != 0 {
		t.Fatalf("result=%+v errors=%v", res, res.Errors)
	}
	if f.listHits != 0 {
		t.Fatalf("event path listed PRs %d times; want 0", f.listHits)
	}
	if len(f.getHits) != 1 || f.getHits[0] != 7 {
		t.Fatalf("PR reads=%v; want [7]", f.getHits)
	}
	if f.filesHits != 1 {
		t.Fatalf("files fetched %d times; want 1 (PR 7 only)", f.filesHits)
	}
	if len(f.labels[7]) != 1 || len(f.labels[8]) != 0 || len(f.comments[8]) != 0 {
		t.Fatalf("labels=%v comments8=%v; PR 8 must be untouched", f.labels, f.comments[8])
	}
	if len(audited) != 1 || audited[0].Repo != "o/r" || audited[0].Number != 7 {
		t.Fatalf("audit=%+v", audited)
	}

	// The delivery's full_name resolves to the configured repo too, and a
	// second event for the same head is a no-op (no files fetch).
	res, err = c.SweepSentinelPR(context.Background(), "O/R", 7, defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("event sweep 2: %v", err)
	}
	if res.Skipped != 1 || f.filesHits != 1 || len(f.comments[7]) != 1 {
		t.Fatalf("same head should skip: res=%+v files=%d", res, f.filesHits)
	}
}

func TestSweepSentinelPREventSkipsUnknownRepoClosedAndDisallowed(t *testing.T) {
	closed := sentinelPR(9, "mallory", "ccc333")
	closed["state"] = "closed"
	f := &sentinelFixture{
		prs:   []map[string]any{closed},
		files: map[int][]map[string]any{9: ownersFile()},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	res, err := c.SweepSentinelPR(context.Background(), "other/repo", 9, defaultSentinelOpts(nil))
	if err != nil || res.Skipped != 1 || len(f.getHits) != 0 {
		t.Fatalf("unknown repo: err=%v res=%+v gets=%v", err, res, f.getHits)
	}
	opts := defaultSentinelOpts(nil)
	opts.RepoAllowed = func(string) bool { return false }
	res, err = c.SweepSentinelPR(context.Background(), "o/r", 9, opts)
	if err != nil || res.Skipped != 1 || len(f.getHits) != 0 {
		t.Fatalf("disallowed repo: err=%v res=%+v gets=%v", err, res, f.getHits)
	}
	res, err = c.SweepSentinelPR(context.Background(), "o/r", 9, defaultSentinelOpts(nil))
	if err != nil || res.Seen != 1 || res.Skipped != 1 || f.filesHits != 0 || len(f.labels[9]) != 0 {
		t.Fatalf("closed PR: err=%v res=%+v files=%d labels=%v", err, res, f.filesHits, f.labels)
	}
	if _, err := c.SweepSentinelPR(context.Background(), "o/r", 0, defaultSentinelOpts(nil)); err == nil {
		t.Fatal("PR number 0 should be rejected")
	}
	res, err = c.SweepSentinelPR(context.Background(), "o/r", 404, defaultSentinelOpts(nil))
	if err != nil || len(res.Errors) != 1 {
		t.Fatalf("missing PR should be an entry in Errors: err=%v res=%+v", err, res)
	}
}

func TestSweepSentinelCadenceEnumeratesFromCacheWithoutListing(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{
			sentinelPR(7, "vjymisal0", "aaa111"),
			sentinelPR(8, "alice", "bbb222"),
			sentinelPR(9, "mallory", "ccc333"),
		},
		files: map[int][]map[string]any{
			7: ownersFile(),
			8: {{"filename": "README.md", "status": "modified", "additions": 1, "deletions": 0, "patch": "+hello\n"}},
			9: ownersFile(),
		},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	cache := &fakeSentinelPRCache{snaps: map[string]forgestate.Snapshot{
		"o/r": {Repo: "o/r", Ready: true, PRs: map[int]forgestate.Item{
			7: cachedPR(7, "aaa111"), 8: cachedPR(8, "bbb222"), 9: cachedPR(9, "ccc333"),
		}},
	}}
	opts := defaultSentinelOpts(nil)
	opts.PRCache = cache

	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if f.listHits != 0 {
		t.Fatalf("cached sweep listed PRs %d times; want 0", f.listHits)
	}
	if res.CachedRepos != 1 || res.ListedRepos != 0 || res.Seen != 3 {
		t.Fatalf("result=%+v", res)
	}
	if len(res.Flagged) != 2 || res.Flagged[0].Number != 7 || res.Flagged[1].Number != 9 || res.Skipped != 1 {
		t.Fatalf("flagged=%+v skipped=%d", res.Flagged, res.Skipped)
	}
	if len(f.getHits) != 3 || f.filesHits != 3 {
		t.Fatalf("first pass reads every cached PR once: gets=%v files=%d", f.getHits, f.filesHits)
	}
	if len(f.labels[7]) != 1 || len(f.labels[9]) != 1 || len(f.labels[8]) != 0 {
		t.Fatalf("labels=%v", f.labels)
	}

	// Second pass, nothing changed: the clean PR is skipped from the heads
	// map with no GitHub call at all; the labelled ones are re-read (the
	// cache item says the label is still on) and skipped before any files
	// fetch, exactly as the listed sweep did.
	gets, files := len(f.getHits), f.filesHits
	cache.snaps["o/r"].PRs[7] = cachedPR(7, "aaa111", "sentinel-alert")
	cache.snaps["o/r"].PRs[9] = cachedPR(9, "ccc333", "sentinel-alert")
	res, err = c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(res.Flagged) != 0 || res.Skipped != 3 || f.filesHits != files {
		t.Fatalf("second pass: res=%+v files=%d->%d", res, files, f.filesHits)
	}
	if len(f.getHits) != gets+2 {
		t.Fatalf("second pass should re-read only the two labelled PRs: gets=%v", f.getHits)
	}

	// A new head on PR 8 is re-read and its files fetched; the others stay
	// skipped.
	f.prs[1] = sentinelPR(8, "alice", "bbb333")
	f.files[8] = ownersFile()
	cache.snaps["o/r"].PRs[8] = cachedPR(8, "bbb333")
	gets, files = len(f.getHits), f.filesHits
	res, err = c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	if len(res.Flagged) != 1 || res.Flagged[0].Number != 8 || f.filesHits != files+1 || len(f.getHits) != gets+3 {
		t.Fatalf("new head: res=%+v files=%d->%d gets=%v", res, files, f.filesHits, f.getHits)
	}
}

func TestSweepSentinelCadenceCachedPRClosedSinceSnapshot(t *testing.T) {
	closed := sentinelPR(7, "vjymisal0", "aaa111")
	closed["state"] = "closed"
	f := &sentinelFixture{prs: []map[string]any{closed}, files: map[int][]map[string]any{7: ownersFile()}}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.PRCache = &fakeSentinelPRCache{snaps: map[string]forgestate.Snapshot{
		"o/r": {Repo: "o/r", Ready: true, Stale: true, PRs: map[int]forgestate.Item{7: cachedPR(7, "aaa111")}},
	}}
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Seen != 1 || res.Skipped != 1 || f.filesHits != 0 || len(f.labels[7]) != 0 || len(res.Errors) != 0 {
		t.Fatalf("closed-since-snapshot PR must not be evaluated: res=%+v files=%d labels=%v", res, f.filesHits, f.labels)
	}
}

func TestSweepSentinelFallsBackToListingWithoutCache(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(7, "vjymisal0", "aaa111")},
		files: map[int][]map[string]any{7: ownersFile()},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	// Registered cache that has not reconciled this repo: listed.
	cache := &fakeSentinelPRCache{snaps: map[string]forgestate.Snapshot{"o/r": {Repo: "o/r", Ready: false}}}
	SetSentinelPRCache(cache)
	defer SetSentinelPRCache(nil)
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if cache.calls != 1 || res.ListedRepos != 1 || res.CachedRepos != 0 || f.listHits != 1 || len(res.Flagged) != 1 {
		t.Fatalf("not-ready cache should list: calls=%d res=%+v listHits=%d", cache.calls, res, f.listHits)
	}
	if len(f.getHits) != 0 {
		t.Fatalf("listed sweep must not GET individual PRs: %v", f.getHits)
	}

	// No cache at all: listed.
	SetSentinelPRCache(nil)
	res, err = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || res.ListedRepos != 1 || f.listHits != 2 {
		t.Fatalf("no cache should list: err=%v res=%+v listHits=%d", err, res, f.listHits)
	}
}

func TestSentinelPREventWanted(t *testing.T) {
	for _, tc := range []struct {
		event, action string
		want          bool
	}{
		{"pull_request", "opened", true},
		{"pull_request", "synchronize", true},
		{"pull_request", "Reopened", true},
		{"pull_request", "labeled", false},
		{"pull_request", "closed", false},
		{"pull_request_review", "submitted", false},
		{"issues", "opened", false},
	} {
		if got := SentinelPREventWanted(tc.event, tc.action); got != tc.want {
			t.Errorf("SentinelPREventWanted(%q,%q)=%v want %v", tc.event, tc.action, got, tc.want)
		}
	}
}

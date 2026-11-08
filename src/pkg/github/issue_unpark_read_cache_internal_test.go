package github

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Entries for items that left the open list are dropped after the repo's
// pass, so the cache tracks only what is still listed.
func TestUnparkSweepPrunesItemsNoLongerListed(t *testing.T) {
	now := time.Now().UTC()
	c, rec, _ := unparkCacheFixture(t, now.Add(-time.Hour))
	unparkCacheSweep(t, c, now)
	if got := c.unparkReads.len(); got != 3 {
		t.Fatalf("cache holds %d items after the first pass, want 3", got)
	}
	c.unparkReads.prune("org", "repo", map[int]bool{41: true})
	if got := c.unparkReads.len(); got != 1 {
		t.Fatalf("cache holds %d items after pruning to one listed item, want 1", got)
	}
	_ = rec
}

// The cache is bounded: past unparkReadMaxItems the oldest read goes.
func TestUnparkReadCacheBounded(t *testing.T) {
	var rc unparkReadCache
	base := time.Now().Add(-time.Hour)
	for i := 0; i < unparkReadMaxItems+10; i++ {
		rc.storeComments(unparkReadKey("o", "r", i), base, time.Time{}, base.Add(time.Duration(i)*time.Second), nil)
	}
	if got := rc.len(); got != unparkReadMaxItems {
		t.Fatalf("cache holds %d items, want the bound %d", got, unparkReadMaxItems)
	}
	if _, ok := rc.entries[unparkReadKey("o", "r", 0)]; ok {
		t.Fatal("the oldest read should have been evicted")
	}
}

// A few very long threads cannot grow the cache past the comment bound.
func TestUnparkReadCacheBoundsComments(t *testing.T) {
	var rc unparkReadCache
	base := time.Now().Add(-time.Hour)
	thread := make([]*gh.IssueComment, unparkReadMaxComments/2+1)
	for i := 0; i < 3; i++ {
		rc.storeComments(unparkReadKey("o", "r", i), base, time.Time{}, base.Add(time.Duration(i)*time.Second), thread)
	}
	if got := rc.len(); got != 1 {
		t.Fatalf("cache holds %d long threads, want only the newest", got)
	}
	if _, ok := rc.entries[unparkReadKey("o", "r", 2)]; !ok {
		t.Fatal("the newest read should be kept")
	}
}

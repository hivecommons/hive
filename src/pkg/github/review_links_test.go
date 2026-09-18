package github

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReviewLinksRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ReviewLinksFile)

	// A ledger that has never been written is empty, not an error: a hive
	// that has posted no reviews must not log a failure every cycle.
	links, err := LoadReviewLinks(path)
	if err != nil {
		t.Fatalf("load missing ledger: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("missing ledger should be empty, got %d", len(links))
	}

	at := time.Date(2026, 9, 18, 5, 51, 0, 0, time.UTC)
	if err := RecordReviewLink(path, "owner/repo", 42, ReviewLink{
		URL:     "https://github.com/owner/repo/pull/42#pullrequestreview-1",
		State:   "commented",
		HeadSHA: "abc123",
		At:      at,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	links, err = LoadReviewLinks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := links[ReviewLinkKey("owner/repo", 42)]
	if !ok {
		t.Fatalf("link not recorded, have %v", links)
	}
	if got.URL != "https://github.com/owner/repo/pull/42#pullrequestreview-1" {
		t.Fatalf("url: %q", got.URL)
	}
	if got.State != "commented" || got.HeadSHA != "abc123" {
		t.Fatalf("fields not persisted: %+v", got)
	}
	if got.Count != 1 {
		t.Fatalf("first review should count 1, got %d", got.Count)
	}
}

func TestRecordReviewLinkCountsRepeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), ReviewLinksFile)

	for i := 0; i < 3; i++ {
		if err := RecordReviewLink(path, "owner/repo", 7, ReviewLink{
			URL: "https://example.test/review/" + string(rune('a'+i)),
			At:  time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	links, err := LoadReviewLinks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := links[ReviewLinkKey("owner/repo", 7)]
	// The count is the duplicate-review signal; the URL always points at the
	// most recent review.
	if got.Count != 3 {
		t.Fatalf("count = %d, want 3", got.Count)
	}
	if got.URL != "https://example.test/review/c" {
		t.Fatalf("url should be the newest, got %q", got.URL)
	}
}

func TestRecordReviewLinkIgnoresEmptyURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), ReviewLinksFile)

	// An entry with no URL would render a pill that goes nowhere.
	if err := RecordReviewLink(path, "owner/repo", 1, ReviewLink{State: "commented"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	links, err := LoadReviewLinks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("empty-URL review should not be recorded, got %v", links)
	}
}

func TestPruneReviewLinksKeepsNewest(t *testing.T) {
	links := map[string]ReviewLink{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxReviewLinks+50; i++ {
		links[ReviewLinkKey("owner/repo", i)] = ReviewLink{
			URL: "https://example.test/r",
			At:  base.Add(time.Duration(i) * time.Minute),
		}
	}
	pruneReviewLinks(links)

	if len(links) != maxReviewLinks {
		t.Fatalf("len = %d, want %d", len(links), maxReviewLinks)
	}
	// The 50 oldest are gone and the newest survives — a queue view asks
	// about recent reviews, so recency is what the bound must preserve.
	if _, ok := links[ReviewLinkKey("owner/repo", 0)]; ok {
		t.Fatal("oldest entry survived pruning")
	}
	if _, ok := links[ReviewLinkKey("owner/repo", maxReviewLinks+49)]; !ok {
		t.Fatal("newest entry was pruned")
	}
}

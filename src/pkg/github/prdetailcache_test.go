package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func testPRDetail(number int, head string, updated time.Time, state string) *gh.PullRequest {
	return &gh.PullRequest{
		Number:              gh.Ptr(number),
		State:               gh.Ptr("open"),
		UpdatedAt:           &gh.Timestamp{Time: updated},
		User:                &gh.User{Login: gh.Ptr("alice")},
		Mergeable:           gh.Ptr(true),
		MergeableState:      gh.Ptr(state),
		MaintainerCanModify: gh.Ptr(true),
		Merged:              gh.Ptr(false),
		MergeCommitSHA:      gh.Ptr("merge-sha"),
		Head:                &gh.PullRequestBranch{SHA: gh.Ptr(head)},
	}
}

func TestPRDetailCacheHitMissAndTTL(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	updated := now.Add(-time.Minute)
	sharedPRDetailCache.put("Org/Repo", 7, testPRDetail(7, "sha", updated, "clean"))
	if _, ok := sharedPRDetailCache.get("org/repo", 7, "sha", updated, time.Hour); !ok {
		t.Fatal("expected fresh matching detail cache hit")
	}
	now = now.Add(time.Hour)
	if _, ok := sharedPRDetailCache.get("org/repo", 7, "sha", updated, time.Hour); ok {
		t.Fatal("expected TTL-expired entry to miss")
	}
	hits, misses, entries := PRDetailCacheStats()
	if hits != 1 || misses != 1 || entries != 1 {
		t.Fatalf("stats = hits %d misses %d entries %d, want 1/1/1", hits, misses, entries)
	}
}

func TestPRDetailCacheInvalidatesOnSHAOrUpdatedAtChange(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	updated := now.Add(-time.Minute)
	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", updated, "clean"))
	if _, ok := sharedPRDetailCache.get("org/repo", 7, "other", updated, time.Hour); ok {
		t.Fatal("expected head SHA change to miss")
	}
	if _, ok := sharedPRDetailCache.get("org/repo", 7, "sha", updated.Add(time.Second), time.Hour); ok {
		t.Fatal("expected updated_at change to miss")
	}
}

func TestPRDetailCacheUnknownMergeableBypasses(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	updated := now.Add(-time.Minute)
	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", updated, "unknown"))
	if _, ok := sharedPRDetailCache.get("org/repo", 7, "sha", updated, time.Hour); ok {
		t.Fatal("expected unknown mergeable_state to bypass cache")
	}
	if _, ok := sharedPRDetailCache.getAny("org/repo", 7, time.Hour); ok {
		t.Fatal("expected default any lookup to bypass unknown mergeable_state")
	}
	if _, ok := sharedPRDetailCache.getAnyAllowUnknown("org/repo", 7, time.Hour); !ok {
		t.Fatal("expected allow-unknown lookup to reuse immutable fields")
	}
}

func TestPRDetailCacheEvictsOldest(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 2)
	defer restore()
	for i, repo := range []string{"org/one", "org/two", "org/three"} {
		now = now.Add(time.Second)
		sharedPRDetailCache.put(repo, i+1, testPRDetail(i+1, "sha", now, "clean"))
	}
	if _, ok := sharedPRDetailCache.getAny("org/one", 1, time.Hour); ok {
		t.Fatal("expected oldest entry to be evicted")
	}
	if _, _, entries := PRDetailCacheStats(); entries != 2 {
		t.Fatalf("entries = %d, want 2", entries)
	}
}

func TestEnrichPRCIUsesPRDetailCache(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	updated := now.Add(-time.Minute)
	var detailHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/pulls/7":
			detailHits.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "updated_at": updated.Format(time.RFC3339),
				"mergeable": true, "mergeable_state": "clean", "maintainer_can_modify": true,
				"head": map[string]any{"sha": "sha"}, "user": map[string]any{"login": "alice"},
			})
		case "/repos/org/repo/commits/sha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}

	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetPRDetailTTLFunc(func() time.Duration { return time.Hour })
	for i := 0; i < 2; i++ {
		pr := PullRequest{Repo: "org/repo", Number: 7, HeadSHA: "sha", UpdatedAt: updated}
		c.enrichPRCI(context.Background(), &pr)
		if pr.Mergeable != MergeableYes || pr.MergeableState != "clean" || !pr.MaintainerCanModify {
			t.Fatalf("iteration %d did not apply cached mergeability: %+v", i, pr)
		}
	}
	if got := detailHits.Load(); got != 1 {
		t.Fatalf("PR detail hits = %d, want 1", got)
	}
}

func TestPRTerminalMergedByReusesUnknownMergeabilityCache(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()

	var detailHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/pulls/7" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		detailHits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "state": "closed", "updated_at": now.Format(time.RFC3339),
			"merged": true, "mergeable_state": "unknown",
			"head":      map[string]any{"sha": "sha"},
			"merged_by": map[string]any{"login": "merger"},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetPRDetailTTLFunc(func() time.Duration { return time.Hour })
	pr := &gh.PullRequest{Number: gh.Ptr(7)}
	for i := 0; i < 2; i++ {
		if got := c.prTerminalMergedBy(context.Background(), "org", "repo", pr); got != "merger" {
			t.Fatalf("iteration %d merged_by = %q, want merger", i, got)
		}
	}
	if got := detailHits.Load(); got != 1 {
		t.Fatalf("PR detail hits = %d, want 1", got)
	}
}

func TestPRTerminalMergedByRefetchesCacheWithoutActor(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", now, "clean"))

	var detailHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/pulls/7" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		detailHits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "state": "closed", "updated_at": now.Format(time.RFC3339),
			"merged": true, "mergeable_state": "unknown",
			"head":      map[string]any{"sha": "sha"},
			"merged_by": map[string]any{"login": "merger"},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetPRDetailTTLFunc(func() time.Duration { return time.Hour })
	pr := &gh.PullRequest{Number: gh.Ptr(7)}
	if got := c.prTerminalMergedBy(context.Background(), "org", "repo", pr); got != "merger" {
		t.Fatalf("merged_by = %q, want merger", got)
	}
	if got := detailHits.Load(); got != 1 {
		t.Fatalf("PR detail hits = %d, want 1", got)
	}
}

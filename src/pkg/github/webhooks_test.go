package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func testWebhookPayload(repo, extra string) []byte {
	return []byte(fmt.Sprintf(`{"repository":{"full_name":%q}%s}`, repo, extra))
}

func TestWebhookInvalidatesPREvents(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	events := []struct{ name, extra string }{
		{"pull_request", `,"pull_request":{"number":7}`},
		{"pull_request_review", `,"pull_request":{"number":7}`},
		{"pull_request_review_comment", `,"pull_request":{"number":7}`},
		{"issue_comment", `,"issue":{"number":7,"pull_request":{}}`},
		{"check_suite", `,"check_suite":{"head_sha":"sha","pull_requests":[{"number":7}]}`},
		{"check_run", `,"check_run":{"head_sha":"sha","pull_requests":[{"number":7}]}`},
		{"status", `,"sha":"sha","pull_requests":[{"number":7}]`},
	}
	for _, tc := range events {
		t.Run(tc.name, func(t *testing.T) {
			sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", now, "clean"))
			c := &Client{org: "org", repos: []string{"repo"}}
			c.prBatchReviews = map[string]map[int]prReviewState{"org/repo": {7: {decision: "APPROVED"}}}
			res, err := c.HandleWebhookInvalidation(context.Background(), tc.name, testWebhookPayload("org/repo", tc.extra))
			if err != nil || res.Invalidations != 1 || res.Ignored {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if _, ok := sharedPRDetailCache.getAny("org/repo", 7, time.Hour, true); ok {
				t.Fatalf("dirty cache hit after %s", tc.name)
			}
			if reviews, _ := c.graphQLBatchReviewStates("org/repo"); len(reviews) != 0 {
				t.Fatalf("batch reviews not invalidated: %+v", reviews)
			}
		})
	}
}

func TestWebhookInvalidationSpecialCases(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	pr7 := testPRDetail(7, "sha", now, "clean")
	pr7.Base = &gh.PullRequestBranch{}
	pr7.Base.Ref = strPtr("main")
	pr7.Head.Ref = strPtr("feature")
	pr8 := testPRDetail(8, "other", now, "clean")
	pr8.Base = &gh.PullRequestBranch{}
	pr8.Base.Ref = strPtr("release")
	pr8.Head.Ref = strPtr("main")
	sharedPRDetailCache.put("org/repo", 7, pr7)
	sharedPRDetailCache.put("org/repo", 8, pr8)
	c := &Client{org: "org", repos: []string{"repo"}}
	res, _ := c.HandleWebhookInvalidation(context.Background(), "push", testWebhookPayload("org/repo", `,"ref":"refs/heads/main"`))
	if res.Invalidations != 2 {
		t.Fatalf("push invalidations=%d want 2", res.Invalidations)
	}
	if _, ok := sharedPRDetailCache.getAny("org/repo", 7, time.Hour, true); ok {
		t.Fatal("base ref PR not dirty")
	}
	if _, ok := sharedPRDetailCache.getAny("org/repo", 8, time.Hour, true); ok {
		t.Fatal("head ref PR not dirty")
	}

	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", now, "clean"))
	res, _ = c.HandleWebhookInvalidation(context.Background(), "check_run", testWebhookPayload("org/repo", `,"check_run":{"head_sha":"sha","pull_requests":[]}`))
	if res.Invalidations != 1 {
		t.Fatalf("head sha fallback invalidations=%d", res.Invalidations)
	}
	res, _ = c.HandleWebhookInvalidation(context.Background(), "issue_comment", testWebhookPayload("org/repo", `,"issue":{"number":7}`))
	if res.Invalidations != 0 {
		t.Fatalf("non-PR issue_comment invalidated")
	}
	res, _ = c.HandleWebhookInvalidation(context.Background(), "pull_request", testWebhookPayload("org/other", `,"pull_request":{"number":7}`))
	if !res.Ignored {
		t.Fatalf("unknown repo not ignored: %+v", res)
	}
}

func TestWebhookHealthTransitionsAndStaleCache(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()
	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", now, "clean"))
	c := &Client{org: "org", repos: []string{"repo"}}
	c.SetWebhookClockForTest(func() time.Time { return now })
	if _, ok := c.cachedPRDetailAny("org/repo", 7); !ok {
		t.Fatal("fresh cache miss")
	}
	now = now.Add(2 * time.Hour)
	if _, ok := c.cachedPRDetailAny("org/repo", 7); ok {
		t.Fatal("stale cache hit before webhook health")
	}
	_, _ = c.HandleWebhookInvalidation(context.Background(), "unknown", testWebhookPayload("org/repo", ``))
	if snap := c.WebhookHealth(15 * time.Minute); !snap.Healthy || snap.Events1h != 1 {
		t.Fatalf("healthy snap=%+v", snap)
	}
	sharedPRDetailCache.put("org/repo", 7, testPRDetail(7, "sha", now, "clean"))
	now = now.Add(2 * time.Hour)
	if _, ok := c.cachedPRDetailAny("org/repo", 7); !ok {
		t.Fatal("clean stale cache should hit while cached healthy")
	}
	if snap := c.WebhookHealth(15 * time.Minute); snap.Healthy {
		t.Fatalf("stale snap=%+v", snap)
	}
	if _, ok := c.cachedPRDetailAny("org/repo", 7); ok {
		t.Fatal("stale webhook health should restore TTL misses")
	}
}

func strPtr(s string) *string { return &s }

func TestWebhookHealthSnapshotJSONNames(t *testing.T) {
	if got := fmt.Sprintf("%#v", WebhookHealthSnapshot{}); !strings.Contains(got, "Invalidations1h") {
		t.Fatal(got)
	}
}

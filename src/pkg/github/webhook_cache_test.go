package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type webhookTestClock struct{ now time.Time }

func (c *webhookTestClock) Now() time.Time { return c.now }

func setupWebhookCacheTest(t *testing.T) (*Client, *webhookTestClock) {
	t.Helper()
	clock := &webhookTestClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv, "org", []string{"repo"}), clock
}

func putWebhookTestPR(c *Client, number int, headSHA, headRef, baseRef string, updated time.Time) {
	pr := testPRDetail(number, headSHA, updated, "clean")
	pr.Head.Ref = gh.Ptr(headRef)
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr(baseRef)}
	c.storePRDetail("repo", number, pr)
	c.prBatchMu.Lock()
	if c.prBatchCheckRuns == nil {
		c.prBatchCheckRuns = map[prBatchCheckRunKey][]*gh.CheckRun{}
	}
	if c.prBatchReviews == nil {
		c.prBatchReviews = map[string]map[int]prReviewState{}
	}
	if c.prBatchReviews["repo"] == nil {
		c.prBatchReviews["repo"] = map[int]prReviewState{}
	}
	c.prBatchCheckRuns[prBatchCheckRunKey{repo: "repo", number: number, headSHA: headSHA}] = []*gh.CheckRun{{Name: gh.Ptr("unit")}}
	c.prBatchReviews["repo"][number] = prReviewState{decision: "APPROVED"}
	c.prBatchMu.Unlock()
}

func seedWebhookTestPRs(c *Client, updated time.Time) {
	putWebhookTestPR(c, 1, "sha-1", "feature-a", "v5", updated)
	putWebhookTestPR(c, 2, "sha-2", "feature-b", "v5", updated)
	putWebhookTestPR(c, 3, "sha-3", "feature-c", "release-1", updated)
}

func webhookTestDirty(repo string, number int) bool {
	t := sharedWebhookTracker
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.dirty[canonicalPRDetailRepo(repo)][number]
	return ok
}

func TestHandleWebhookEventInvalidatesPerEventType(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		payload string
		want    []int
	}{
		{"pull_request", "pull_request", `{"action":"synchronize","number":2,"pull_request":{"number":2},"repository":{"full_name":"org/repo"}}`, []int{2}},
		{"pull_request number only", "pull_request", `{"action":"labeled","number":1,"repository":{"full_name":"org/repo"}}`, []int{1}},
		{"pull_request_review", "pull_request_review", `{"action":"submitted","pull_request":{"number":3},"repository":{"full_name":"org/repo"}}`, []int{3}},
		{"issue_comment on PR", "issue_comment", `{"action":"created","issue":{"number":2,"pull_request":{"url":"x"}},"repository":{"full_name":"org/repo"}}`, []int{2}},
		{"issue_comment on issue", "issue_comment", `{"action":"created","issue":{"number":2},"repository":{"full_name":"org/repo"}}`, []int{}},
		{"check_suite with PRs", "check_suite", `{"action":"completed","check_suite":{"head_sha":"unknown","pull_requests":[{"number":1}]},"repository":{"full_name":"org/repo"}}`, []int{1}},
		{"check_suite fork by SHA", "check_suite", `{"action":"completed","check_suite":{"head_sha":"sha-3","pull_requests":[]},"repository":{"full_name":"org/repo"}}`, []int{3}},
		{"check_run", "check_run", `{"action":"completed","check_run":{"head_sha":"sha-2","pull_requests":[{"number":2}]},"repository":{"full_name":"org/repo"}}`, []int{2}},
		{"status", "status", `{"sha":"sha-1","state":"success","repository":{"full_name":"org/repo"}}`, []int{1}},
		{"unrelated event", "issues", `{"action":"opened","issue":{"number":9},"repository":{"full_name":"org/repo"}}`, []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, clock := setupWebhookCacheTest(t)
			seedWebhookTestPRs(c, clock.now.Add(-time.Minute))

			res, err := c.HandleWebhookEvent(tt.event, []byte(tt.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !res.KnownRepo {
				t.Fatalf("org/repo should be a known repo")
			}
			if !reflect.DeepEqual(res.Invalidated, tt.want) {
				t.Fatalf("invalidated = %v, want %v", res.Invalidated, tt.want)
			}
			wantSet := map[int]bool{}
			for _, n := range tt.want {
				wantSet[n] = true
			}
			reviews, _ := c.graphQLBatchReviewStates("repo")
			for n := 1; n <= 3; n++ {
				_, cached := sharedPRDetailCache.getAny("repo", n, time.Hour)
				_, checks := c.graphQLBatchCheckRuns("repo", n, fmt.Sprintf("sha-%d", n))
				_, review := reviews[n]
				if cached == wantSet[n] || checks == wantSet[n] || review == wantSet[n] {
					t.Fatalf("PR %d: detail cached=%v checks=%v review=%v, invalidated=%v", n, cached, checks, review, wantSet[n])
				}
				if webhookTestDirty("repo", n) != wantSet[n] {
					t.Fatalf("PR %d dirty = %v, want %v", n, !wantSet[n], wantSet[n])
				}
			}
			if got := WebhookHealthSnapshot(); got.Events1h != 1 || got.Invalidations1h != len(tt.want) || !got.Healthy {
				t.Fatalf("snapshot = %+v, want 1 event, %d invalidations, healthy", got, len(tt.want))
			}
		})
	}
}

func TestHandleWebhookEventPushFansOutToBaseAndHeadRefs(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want []int
	}{
		{"base ref", "refs/heads/v5", []int{1, 2}},
		{"head ref", "refs/heads/feature-c", []int{3}},
		{"other base", "refs/heads/release-1", []int{3}},
		{"unmatched branch", "refs/heads/nobody", []int{}},
		{"tag", "refs/tags/v5", []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, clock := setupWebhookCacheTest(t)
			seedWebhookTestPRs(c, clock.now.Add(-time.Minute))
			payload := fmt.Sprintf(`{"ref":%q,"after":"new","repository":{"full_name":"Org/Repo"}}`, tt.ref)
			res, err := c.HandleWebhookEvent("push", []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Invalidated, tt.want) {
				t.Fatalf("push %s invalidated %v, want %v", tt.ref, res.Invalidated, tt.want)
			}
		})
	}
}

func TestHandleWebhookEventIgnoresUnknownRepo(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	seedWebhookTestPRs(c, clock.now.Add(-time.Minute))
	res, err := c.HandleWebhookEvent("pull_request", []byte(`{"number":1,"pull_request":{"number":1},"repository":{"full_name":"other/repo"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.KnownRepo || len(res.Invalidated) != 0 {
		t.Fatalf("unknown repo result = %+v, want ignored", res)
	}
	if _, ok := sharedPRDetailCache.getAny("repo", 1, time.Hour); !ok {
		t.Fatal("unknown-repo event must not invalidate the managed repo's cache")
	}
	if got := WebhookHealthSnapshot(); got.Events1h != 0 || got.Healthy || got.LastEventAt != nil {
		t.Fatalf("unknown-repo event counted toward health: %+v", got)
	}
	if _, err := c.HandleWebhookEvent("push", []byte(`not json`)); err == nil {
		t.Fatal("malformed payload should error")
	}
}

func TestWebhookHealthyStaleTransitions(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	SetWebhookHealthWindow(10 * time.Minute)

	if healthy, becameStale := ObserveWebhookHealth(); healthy || becameStale {
		t.Fatalf("no events: healthy=%v becameStale=%v, want false/false", healthy, becameStale)
	}
	if _, err := c.HandleWebhookEvent("ping", []byte(`{"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	if healthy, becameStale := ObserveWebhookHealth(); !healthy || becameStale {
		t.Fatalf("fresh event: healthy=%v becameStale=%v, want true/false", healthy, becameStale)
	}
	clock.now = clock.now.Add(9 * time.Minute)
	if healthy, _ := ObserveWebhookHealth(); !healthy {
		t.Fatal("event inside the window should stay healthy")
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if healthy, becameStale := ObserveWebhookHealth(); healthy || !becameStale {
		t.Fatalf("past the window: healthy=%v becameStale=%v, want false/true", healthy, becameStale)
	}
	if _, becameStale := ObserveWebhookHealth(); becameStale {
		t.Fatal("healthy → stale must be reported once, not every observation")
	}
	snap := WebhookHealthSnapshot()
	if snap.Healthy || snap.LastEventAt == nil || snap.Events1h != 1 {
		t.Fatalf("stale snapshot = %+v", snap)
	}
	clock.now = clock.now.Add(time.Hour)
	if snap := WebhookHealthSnapshot(); snap.Events1h != 0 {
		t.Fatalf("events_1h should age out after an hour, got %d", snap.Events1h)
	}
	if _, err := c.HandleWebhookEvent("ping", []byte(`{"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	if healthy, becameStale := ObserveWebhookHealth(); !healthy || becameStale {
		t.Fatalf("recovered: healthy=%v becameStale=%v, want true/false", healthy, becameStale)
	}
}

func TestPRDetailCacheServesCleanPRsPastTTLWhileWebhooksHealthy(t *testing.T) {
	c, clock := setupWebhookCacheTest(t)
	SetWebhookHealthWindow(10 * time.Minute)
	updated := clock.now.Add(-time.Minute)
	seedWebhookTestPRs(c, updated)
	clock.now = clock.now.Add(2 * time.Hour)

	if _, ok := sharedPRDetailCache.get("repo", 1, "sha-1", updated, time.Hour); ok {
		t.Fatal("without webhooks an entry past TTL must miss")
	}
	if _, err := c.HandleWebhookEvent("pull_request", []byte(`{"number":2,"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := sharedPRDetailCache.get("repo", 1, "sha-1", updated, time.Hour); !ok {
		t.Fatal("clean PR should be served past TTL while webhooks are healthy")
	}
	if _, ok := sharedPRDetailCache.get("repo", 1, "other-sha", updated, time.Hour); ok {
		t.Fatal("a head SHA change must still miss even when webhooks are healthy")
	}
	if _, ok := sharedPRDetailCache.get("repo", 2, "sha-2", updated, time.Hour); ok {
		t.Fatal("dirty PR must re-enrich")
	}
	clock.now = clock.now.Add(11 * time.Minute)
	if _, ok := sharedPRDetailCache.get("repo", 1, "sha-1", updated, time.Hour); ok {
		t.Fatal("stale webhooks must fall back to the TTL")
	}
}

func TestGraphQLPRBatchSkipsCleanRepoWhileWebhooksHealthy(t *testing.T) {
	clock := &webhookTestClock{now: time.Now()}
	defer resetPRDetailCacheForTest(clock.Now, 128)()
	defer resetWebhookTrackerForTest(clock.Now)()
	SetWebhookHealthWindow(10 * time.Minute)

	var gqlCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4999,"reset":1791500400}}}`)
			return
		}
		if r.URL.Path != "/graphql" {
			t.Errorf("unexpected path %s", r.URL.Path)
			return
		}
		gqlCalls.Add(1)
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}},"rateLimit":{"cost":1,"remaining":4999,"resetAt":"2026-10-08T23:00:00Z"}}}`,
			gqlPRNodeJSON(1, "CLEAN", "MERGEABLE", "sha-1", "completed", "success"))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "repo")
	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "repo")
	if got := gqlCalls.Load(); got != 2 {
		t.Fatalf("without webhooks every scan runs the batch: calls = %d, want 2", got)
	}

	if _, err := c.HandleWebhookEvent("ping", []byte(`{"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "repo")
	if got := gqlCalls.Load(); got != 2 {
		t.Fatalf("healthy + clean repo should reuse the batch: calls = %d, want 2", got)
	}

	clock.now = clock.now.Add(time.Second)
	if _, err := c.HandleWebhookEvent("check_run", []byte(`{"check_run":{"head_sha":"sha-1","pull_requests":[{"number":1}]},"repository":{"full_name":"org/repo"}}`)); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Second)
	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "repo")
	if got := gqlCalls.Load(); got != 3 {
		t.Fatalf("dirty PR should re-run the batch: calls = %d, want 3", got)
	}
	if webhookTestDirty("repo", 1) {
		t.Fatal("successful batch should clear the dirty mark")
	}
	if _, ok := c.graphQLBatchCheckRuns("repo", 1, "sha-1"); !ok {
		t.Fatal("re-run batch should repopulate check runs")
	}
	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "repo")
	if got := gqlCalls.Load(); got != 3 {
		t.Fatalf("clean again after re-enrichment: calls = %d, want 3", got)
	}
	info, err := c.RateLimits(context.Background())
	if err == nil && info.GraphQLPRBatch.WebhookSkips != 2 {
		t.Fatalf("webhook_skips = %d, want 2", info.GraphQLPRBatch.WebhookSkips)
	}
}

func TestWebhookDirtyMarkSurvivesInFlightFetch(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	defer resetWebhookTrackerForTest(clock.Now)()
	started := clock.now
	clock.now = clock.now.Add(time.Second)
	sharedWebhookTracker.markDirty([]string{"repo"}, 4)
	sharedWebhookTracker.clearDirtyBefore("repo", 4, started)
	if !webhookTestDirty("repo", 4) {
		t.Fatal("an event that lands after the fetch started must stay dirty")
	}
	sharedWebhookTracker.clearDirtyBefore("repo", 4, clock.now.Add(time.Second))
	if webhookTestDirty("repo", 4) {
		t.Fatal("a mark older than the fetch should clear")
	}
}

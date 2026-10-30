package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchWebhookItemSnapshotReadsOnePRAndOneIssue pins the targeted
// re-read the stage tracker makes after a webhook: one GET per item, the
// display fields the tracker renders, the PR detail cache refreshed, and
// an issue that turns out to be a PR flagged as such.
func TestFetchWebhookItemSnapshotReadsOnePRAndOneIssue(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	// Earlier tests in the binary may leave the process-wide core budget in
	// its critical band or engage rate-limit caution. This test exercises the
	// reads, not either discretionary-read gate.
	resetRESTAccountingForTest(t, clock.now)
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	updated := clock.now.Add(-time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/repos/org/repo/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "Relabelled", "state": "open", "draft": true,
				"user":     map[string]any{"login": "mina"},
				"labels":   []map[string]any{{"name": "stage/050-review-pending"}, {"name": "kind/bug"}},
				"html_url": "https://github.com/org/repo/pull/42", "updated_at": updated.Format(time.RFC3339),
				"head": map[string]any{"sha": "abc"}, "body": "x",
			})
		case "/repos/org/repo/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "title": "Login loops", "state": "closed",
				"user":     map[string]any{"login": "karibhou"},
				"labels":   []map[string]any{{"name": "needs-human"}},
				"html_url": "https://github.com/org/repo/issues/7", "updated_at": updated.Format(time.RFC3339),
			})
		case "/repos/org/repo/issues/9":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 9, "title": "Actually a PR", "state": "open",
				"pull_request": map[string]any{"url": "https://api.github.com/repos/org/repo/pulls/9"},
			})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	ctx := context.Background()

	pr, err := c.FetchWebhookItemSnapshot(ctx, "repo", 42, true)
	if err != nil {
		t.Fatal(err)
	}
	want := WebhookItemSnapshot{Repo: "repo", Number: 42, IsPR: true, Title: "Relabelled", Author: "mina",
		Labels: []string{"stage/050-review-pending", "kind/bug"}, Draft: true, State: "open", UpdatedAt: updated, URL: "https://github.com/org/repo/pull/42"}
	// The governor-side classification is covered by webhook_item_patch_test.
	if pr.Patch.Bucket != WebhookBucketSidelined {
		t.Fatalf("draft PR bucket = %q, want sidelined", pr.Patch.Bucket)
	}
	if pr.HeadSHA != "abc" || pr.FetchedAt.IsZero() {
		t.Fatalf("pr snapshot must carry head SHA and fetch time: %+v", pr)
	}
	pr.Patch, pr.HeadSHA, pr.FetchedAt = WebhookItemPatch{}, "", time.Time{}
	if !reflect.DeepEqual(pr, want) {
		t.Fatalf("pr snapshot = %+v, want %+v", pr, want)
	}
	if !pr.Open() {
		t.Fatal("open PR must report Open()")
	}
	if cached, ok := c.cachedPRDetailAny("repo", 42); !ok || cached.GetTitle() != "Relabelled" {
		t.Fatalf("PR detail cache not refreshed by the targeted read: ok=%v", ok)
	}

	issue, err := c.FetchWebhookItemSnapshot(ctx, "repo", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if issue.IsPR || issue.Open() || issue.Title != "Login loops" || issue.Author != "karibhou" || !reflect.DeepEqual(issue.Labels, []string{"needs-human"}) || !issue.UpdatedAt.Equal(updated) {
		t.Fatalf("issue snapshot = %+v", issue)
	}
	if asPR, err := c.FetchWebhookItemSnapshot(ctx, "repo", 9, false); err != nil || !asPR.IsPR {
		t.Fatalf("issue that is a PR: snap=%+v err=%v", asPR, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("GitHub calls = %d, want exactly one per item", got)
	}
	var nilClient *Client
	if _, err := nilClient.FetchWebhookItemSnapshot(ctx, "repo", 1, true); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v", err)
	}
}

// TestFetchWebhookItemSnapshotRefusesWhenBudgetCritical: a display refresh
// is discretionary; in the critical budget band it costs nothing.
func TestFetchWebhookItemSnapshotRefusesWhenBudgetCritical(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("critical budget must not reach GitHub: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.rateLimits.now = func() time.Time { return now }
	c.SetAPIBudgetThresholds(800, 250)
	c.rateLimits.mu.Lock()
	if c.rateLimits.windows == nil {
		c.rateLimits.windows = map[string]rateLimitWindow{}
	}
	c.rateLimits.windows[githubCoreRateLimitResource] = rateLimitWindow{limit: 5000, remaining: 10, reset: now.Add(time.Hour), observedAt: now}
	c.rateLimits.mu.Unlock()
	if mode, _ := c.APIBudgetMode(); mode != APIBudgetCritical {
		t.Fatalf("budget mode = %v, want critical", mode)
	}
	if _, err := c.FetchWebhookItemSnapshot(context.Background(), "repo", 42, true); !errors.Is(err, ErrAPIBudgetCritical) {
		t.Fatalf("err = %v, want ErrAPIBudgetCritical", err)
	}
}

// TestWebhookHealthCountsTargetedRefreshesAndTrackerInvalidations pins
// the two additive /api/status webhooks{} counters and their 1h window.
func TestWebhookHealthCountsTargetedRefreshesAndTrackerInvalidations(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	RecordWebhookTargetedRefreshes(3)
	RecordWebhookTrackerInvalidations(2)
	RecordWebhookTargetedRefreshes(0)
	snap := WebhookHealthSnapshot()
	if snap.TargetedRefreshes1h != 3 || snap.TrackerInvalidations1h != 2 {
		t.Fatalf("snapshot = %+v, want 3 refreshes / 2 invalidations", snap)
	}
	clock.now = clock.now.Add(59 * time.Minute)
	RecordWebhookTrackerInvalidations(1)
	clock.now = clock.now.Add(2 * time.Minute)
	snap = WebhookHealthSnapshot()
	if snap.TargetedRefreshes1h != 0 || snap.TrackerInvalidations1h != 1 {
		t.Fatalf("after the hour: %+v, want 0 refreshes / 1 invalidation", snap)
	}
	data, _ := json.Marshal(snap)
	for _, k := range []string{`"targeted_refreshes_1h":0`, `"tracker_invalidations_1h":1`} {
		if !strings.Contains(string(data), k) {
			t.Fatalf("json %s lacks %s", data, k)
		}
	}
}

// TestHandleWebhookEventNamesTouchedIssues: `issues` and `issue_comment`
// deliveries on a non-PR issue name it for the tracker re-read whatever the
// action; PR-shaped issue_comment deliveries stay PR invalidations only.
func TestHandleWebhookEventNamesTouchedIssues(t *testing.T) {
	c, _ := setupWebhookCacheTest(t)
	cases := []struct {
		event, body string
		touched     []int
		invalidated []int
	}{
		{"issues", `{"action":"assigned","issue":{"number":7},"repository":{"full_name":"org/repo"}}`, []int{7}, []int{}},
		{"issues", `{"action":"labeled","issue":{"number":8},"repository":{"full_name":"org/repo"}}`, []int{8}, []int{}},
		{"issue_comment", `{"action":"created","issue":{"number":9},"repository":{"full_name":"org/repo"}}`, []int{9}, []int{}},
		{"issue_comment", `{"action":"created","issue":{"number":10,"pull_request":{}},"repository":{"full_name":"org/repo"}}`, nil, []int{10}},
		{"pull_request", `{"action":"labeled","pull_request":{"number":11},"repository":{"full_name":"org/repo"}}`, nil, []int{11}},
	}
	for _, tc := range cases {
		res, err := c.HandleWebhookEvent(tc.event, []byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if !res.KnownRepo || res.ConfiguredRepo != "repo" {
			t.Fatalf("%s: res = %+v, want known repo 'repo'", tc.event, res)
		}
		if !reflect.DeepEqual(res.TouchedIssues, tc.touched) || !reflect.DeepEqual(res.Invalidated, tc.invalidated) {
			t.Fatalf("%s %s: touched=%v invalidated=%v, want %v / %v", tc.event, tc.body, res.TouchedIssues, res.Invalidated, tc.touched, tc.invalidated)
		}
	}
}

// TestFetchWebhookItemSnapshotRefusesDuringRateLimitCaution: while GitHub
// is pacing the hive after a rate-limit refusal the targeted GET stands
// down; the next eval cycle re-reads the item anyway.
func TestFetchWebhookItemSnapshotRefusesDuringRateLimitCaution(t *testing.T) {
	// Isolate from any budget state another test left in the shared REST
	// accounting; the budget check runs before the caution check.
	resetRESTAccountingForTest(t, time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("caution window must not reach GitHub: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	t.Cleanup(ResetRateLimitPacingForTest)
	sharedSlowStartState.mu.Lock()
	sharedSlowStartState.cautiousUntil = time.Now().Add(time.Hour)
	sharedSlowStartState.mu.Unlock()
	if !RateLimitCautionEngaged() {
		t.Fatal("caution window not engaged")
	}
	if _, err := c.FetchWebhookItemSnapshot(context.Background(), "repo", 42, true); !errors.Is(err, ErrRateLimitCaution) {
		t.Fatalf("err = %v, want ErrRateLimitCaution", err)
	}
}

// A webhook re-read of a blocked issue records the same row facts the
// enumeration does, so a patched row cannot open liftBlockedMarkerless on
// an issue GitHub still reports an open "blocked by" on.
func TestFetchWebhookItemSnapshotBlockedRowFacts(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	resetRESTAccountingForTest(t, clock.now)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/repo/issues/5" {
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 5, "title": "Review backlog (style): x", "state": "open",
			"user":                       map[string]any{"login": "hive-app[bot]"},
			"labels":                     []map[string]any{{"name": "blocked"}},
			"body":                       "Finding: x\n\n" + reviewBacklogFooter + "\n",
			"issue_dependencies_summary": map[string]any{"blocked_by": 1, "total_blocked_by": 1},
		})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	snap, err := c.FetchWebhookItemSnapshot(context.Background(), "repo", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	row := snap.Patch.WorkIssue
	if row == nil || !row.ReviewBacklog || row.OpenBlockedBy != 1 {
		t.Fatalf("blocked row = %+v, want ReviewBacklog and OpenBlockedBy=1", row)
	}
}

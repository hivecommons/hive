package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowReviewCadence makes both unprompted cadences effectively never, so a
// test can prove a pass happened because of a wake, not a timer.
func slowReviewCadence(t *testing.T) {
	t.Helper()
	oldPoll, oldFallback := reviewRequestPollInterval, reviewRequestFallbackInterval
	reviewRequestPollInterval, reviewRequestFallbackInterval = time.Hour, time.Hour
	t.Cleanup(func() {
		reviewRequestPollInterval, reviewRequestFallbackInterval = oldPoll, oldFallback
	})
}

const reviewWakePayload = `{"action":"submitted","pull_request":{"number":1},"repository":{"full_name":"o/r"}}`

// A review-shaped webhook delivery wakes the watcher: the queued request for
// that PR is submitted without waiting for the poll or fallback timer.
func TestReviewRequestWatcher_WebhookEventWakesQueuedRequest(t *testing.T) {
	t.Cleanup(ResetWebhookTrackerForTest())
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDirForLoop(t)
	slowReviewCadence(t)

	// Dropped BEFORE the watcher starts so the dir notification cannot be
	// what delivers it.
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 1, Event: "approve", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartReviewRequestWatcher(ctx, func(string, int) error { return nil }, nil)
	drainAfter(t, cancel, done)

	// Give the loop a chance to misbehave: the review must NOT be posted
	// before a wake. A short deadline bounds the negative check.
	quiet := time.NewTimer(150 * time.Millisecond)
	defer quiet.Stop()
	select {
	case <-quiet.C:
	case <-done:
		t.Fatal("watcher exited before any wake")
	}
	if reviewed.Load() != 0 {
		t.Fatalf("request processed before any wake (reviewed=%d); cadence seam is not holding", reviewed.Load())
	}

	res, err := c.HandleWebhookEvent("pull_request_review", []byte(reviewWakePayload))
	if err != nil {
		t.Fatal(err)
	}
	if !res.WokeReviewWatcher {
		t.Fatal("pull_request_review delivery must report waking the review watcher")
	}
	if !waitFor(t, 5*time.Second, func() bool { return reviewed.Load() > 0 }) {
		t.Fatal("webhook wake did not trigger a watcher pass")
	}
}

// A request file dropped while the watcher is idle is picked up by the
// directory notification, not a timer, so agent-facing latency does not
// grow with the five-minute webhook fallback.
func TestReviewRequestWatcher_DirNotificationWakesNewRequest(t *testing.T) {
	t.Cleanup(ResetWebhookTrackerForTest())
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDirForLoop(t)
	slowReviewCadence(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartReviewRequestWatcher(ctx, func(string, int) error { return nil }, nil)
	drainAfter(t, cancel, done)

	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 3, Event: "approve", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return reviewed.Load() > 0 }) {
		t.Fatal("request-dir notification did not wake the watcher")
	}
}

// Degraded webhooks restore the legacy poll: with a healthy-only fallback of
// an hour and no webhook deliveries, the short poll still processes a request
// that nothing woke the watcher for.
func TestReviewRequestWatcher_DegradedWebhooksRestorePolling(t *testing.T) {
	t.Cleanup(ResetWebhookTrackerForTest())
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDirForLoop(t)
	oldFallback := reviewRequestFallbackInterval
	reviewRequestFallbackInterval = time.Hour
	t.Cleanup(func() { reviewRequestFallbackInterval = oldFallback })
	fastTick(t, func(d time.Duration) { reviewRequestPollInterval = d }, reviewRequestPollInterval)

	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 4, Event: "approve", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartReviewRequestWatcher(ctx, func(string, int) error { return nil }, nil)
	drainAfter(t, cancel, done)

	if !waitFor(t, 5*time.Second, func() bool { return reviewed.Load() > 0 }) {
		t.Fatal("degraded webhooks must fall back to the poll cadence")
	}
}

func TestReviewRequestScanInterval(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	t.Cleanup(ResetWebhookTrackerForTestWithClock(clock))
	poll, fallback := 10*time.Second, 5*time.Minute

	if got := reviewRequestScanInterval(true, poll, fallback); got != poll {
		t.Fatalf("no webhook ever seen: want poll %v, got %v", poll, got)
	}
	RecordWebhookEventForTest("o/r")
	if got := reviewRequestScanInterval(true, poll, fallback); got != fallback {
		t.Fatalf("healthy webhooks: want fallback %v, got %v", fallback, got)
	}
	if got := reviewRequestScanInterval(false, poll, fallback); got != poll {
		t.Fatalf("no dir notification: want poll %v even with healthy webhooks, got %v", poll, got)
	}
	now = now.Add(DefaultWebhookHealthWindow + time.Second)
	if got := reviewRequestScanInterval(true, poll, fallback); got != poll {
		t.Fatalf("webhooks went stale: want poll %v, got %v", poll, got)
	}
}

func TestReviewRequestWakeEvent(t *testing.T) {
	cases := []struct {
		event, action string
		want          bool
	}{
		{"issue_comment", "created", true},
		{"pull_request_review", "submitted", true},
		{"pull_request_review_comment", "created", true},
		{"issues", "labeled", true},
		{"issues", "unlabeled", true},
		{"issues", "opened", false},
		{"pull_request", "synchronize", false},
		{"push", "", false},
	}
	for _, tc := range cases {
		p := webhookEventPayload{Action: tc.action}
		if got := reviewRequestWakeEvent(tc.event, p); got != tc.want {
			t.Errorf("%s/%s: want %v, got %v", tc.event, tc.action, tc.want, got)
		}
	}
}

// A --revise request for a repo outside review.revise_repos is policy, not a
// transient failure (#13252): it is denied once, moved aside, and never
// re-attempted on later passes.
func TestReviewRequestWatcher_ReviseNotAllowlistedIsTerminal(t *testing.T) {
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	c.SetReviseRepos([]string{"o/other"})
	dir := withReviewDir(t)

	path, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: "comment", Body: "needs work", Agent: "a", Revise: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.ProcessReviewRequestsOnce(ctx)
	c.ProcessReviewRequestsOnce(ctx)

	if reviewed.Load() != 0 {
		t.Fatalf("non-allowlisted revise must not post a review (reviewed=%d)", reviewed.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("request file must be moved aside, stat err=%v", err)
	}
	if _, err := os.Stat(path + ".denied"); err != nil {
		t.Fatalf("request must be denied (.denied), got %v", err)
	}
	result, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "not allowlisted for in-place review revision") {
		t.Fatalf("result must name the policy refusal, got %s", result)
	}
	if c.reviewRetries.m != nil {
		if _, tracked := c.reviewRetries.m[path]; tracked {
			t.Fatal("denied request must not be left in the retry tracker")
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".result.json") && !strings.HasSuffix(e.Name(), ".denied") {
			t.Fatalf("unexpected live request left behind: %s", filepath.Join(dir, e.Name()))
		}
	}
}

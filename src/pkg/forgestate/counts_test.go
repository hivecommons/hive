package forgestate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeOutcomeAPI stands in for the hourly GitHub Search reconcile.
type fakeOutcomeAPI struct {
	mu     sync.Mutex
	merged map[string]int
	closed map[string]int
	err    error
	calls  int
}

func newOutcomeAPI() *fakeOutcomeAPI {
	return &fakeOutcomeAPI{merged: map[string]int{}, closed: map[string]int{}}
}

func (f *fakeOutcomeAPI) set(repo string, merged, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged[repo], f.closed[repo] = merged, closed
}

func (f *fakeOutcomeAPI) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeOutcomeAPI) reconcile(_ context.Context, repo string) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, 0, f.err
	}
	return f.merged[repo], f.closed[repo], nil
}

func (f *fakeOutcomeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newCountsHarness(t *testing.T) (*Counts, *harness, *fakeOutcomeAPI) {
	t.Helper()
	h := newHarness(t)
	api := newOutcomeAPI()
	c := NewCounts(CountsOptions{
		Repos:     []string{"o/r"},
		Open:      h.store,
		Reconcile: api.reconcile,
		Author:    "hive-bot[bot]",
		Now:       h.clock.Now,
	})
	return c, h, api
}

func TestCountsOpenFromCache(t *testing.T) {
	c, h, _ := newCountsHarness(t)

	if got, ok := c.Open("o/r"); !ok || got.Ready {
		t.Fatalf("before the first reconcile Open = %+v ok=%v, want not ready", got, ok)
	}
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0), issue(3, t0)}, []Item{pr(10, t0, "a"), pr(11, t0, "b")}, "")
	mustReconcile(t, h, "o/r")

	got, ok := c.Open("o/r")
	if !ok || !got.Ready || got.Issues != 3 || got.PRs != 2 || got.Stale {
		t.Fatalf("Open = %+v ok=%v, want ready 3 issues / 2 PRs", got, ok)
	}
	if _, ok := c.Open("o/unknown"); ok {
		t.Fatal("Open must report ok=false for a repo the cache does not hold")
	}

	// A closed issue leaves the cache on its webhook and the count follows.
	h.fetcher.setItem(closedIssue(2, t0.Add(time.Second)))
	if err := h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "d1", Event: "issues", Action: "closed", Kind: KindIssue, Number: 2}); err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	if got, _ := c.Open("o/r"); got.Issues != 2 {
		t.Fatalf("Issues after close = %d, want 2", got.Issues)
	}

	// A 403 on reconcile keeps the last-good counts and exposes stale_since.
	h.clock.Advance(time.Minute)
	h.fetcher.failList(errors.New("403 secondary rate limit"))
	_ = h.store.Reconcile(context.Background(), "o/r")
	got, _ = c.Open("o/r")
	if got.Issues != 2 || got.PRs != 2 {
		t.Fatalf("counts after 403 = %d/%d, want last good 2/2", got.Issues, got.PRs)
	}
	if !got.Stale || !got.StaleSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("stale = %v since %v, want stale since %v", got.Stale, got.StaleSince, t0.Add(time.Minute))
	}
}

func TestCountsNoOpenSourceReportsNotOK(t *testing.T) {
	c := NewCounts(CountsOptions{Repos: []string{"o/r"}})
	if _, ok := c.Open("o/r"); ok {
		t.Fatal("Open without an OpenSource must report ok=false")
	}
	if c.ReconcileDue() {
		t.Fatal("ReconcileDue without a reconciler must be false")
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile without a reconciler: %v", err)
	}
}

func TestCountsIncrementOnEvents(t *testing.T) {
	c, _, api := newCountsHarness(t)
	api.set("o/r", 10, 20)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cases := []struct {
		name          string
		event         string
		payload       string
		wantMoved     bool
		wantMerged    int
		wantClosedIss int
	}{
		{"merged PR by hive", "pull_request", `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":1,"merged":true,"user":{"login":"hive-bot[bot]"}}}`, true, 11, 20},
		{"PR closed without merge", "pull_request", `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":2,"merged":false,"user":{"login":"hive-bot[bot]"}}}`, false, 11, 20},
		{"merged PR by a human", "pull_request", `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":3,"merged":true,"user":{"login":"alice"}}}`, false, 11, 20},
		{"app login without suffix", "pull_request", `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":4,"merged":true,"user":{"login":"hive-bot"}}}`, true, 12, 20},
		{"closed issue by hive", "issues", `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":5,"user":{"login":"hive-bot[bot]"}}}`, true, 12, 21},
		{"reopened issue", "issues", `{"action":"reopened","repository":{"full_name":"o/r"},"issue":{"number":5,"user":{"login":"hive-bot[bot]"}}}`, false, 12, 21},
		{"closed issue that is a PR", "issues", `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":6,"pull_request":{},"user":{"login":"hive-bot[bot]"}}}`, false, 12, 21},
		{"unrelated event", "issue_comment", `{"action":"created","repository":{"full_name":"o/r"},"issue":{"number":5,"user":{"login":"hive-bot[bot]"}}}`, false, 12, 21},
		{"repo case-insensitive", "issues", `{"action":"closed","repository":{"full_name":"O/R"},"issue":{"number":7,"user":{"login":"HIVE-BOT[bot]"}}}`, true, 12, 22},
	}
	for _, tc := range cases {
		moved, err := c.NoteWebhook(tc.event, []byte(tc.payload))
		if err != nil {
			t.Fatalf("%s: NoteWebhook: %v", tc.name, err)
		}
		if moved != tc.wantMoved {
			t.Errorf("%s: moved = %v, want %v", tc.name, moved, tc.wantMoved)
		}
		got := c.Outcomes()
		if got.MergedPRs != tc.wantMerged || got.ClosedIssues != tc.wantClosedIss {
			t.Errorf("%s: outcomes = %d/%d, want %d/%d", tc.name, got.MergedPRs, got.ClosedIssues, tc.wantMerged, tc.wantClosedIss)
		}
	}
	got := c.Outcomes()
	if !got.Ready || got.Stale || got.Source != SourceWebhook {
		t.Fatalf("outcomes after events = %+v, want ready, not stale, source webhook", got)
	}
	if _, err := c.NoteWebhook("issues", []byte("{not json")); err == nil {
		t.Fatal("undecodable body must error")
	}
	if api.callCount() != 1 {
		t.Fatalf("webhook increments made %d API calls, want 1 (the reconcile)", api.callCount())
	}
}

func TestCountsEventsBeforeReconcileAreNotReady(t *testing.T) {
	c, _, _ := newCountsHarness(t)
	if !c.NoteClosed("o/r", KindIssue, false, "hive-bot[bot]") {
		t.Fatal("NoteClosed must move the counter")
	}
	got := c.Outcomes()
	if got.Ready || got.ClosedIssues != 1 {
		t.Fatalf("outcomes = %+v, want not ready with 1 closed", got)
	}
	// A persisted baseline makes the counts usable before GitHub answers.
	c.Seed("o/r", 4, 9, t0)
	got = c.Outcomes()
	if !got.Ready || got.MergedPRs != 4 || got.ClosedIssues != 9 || !got.LastGoodAt.Equal(t0) {
		t.Fatalf("seeded outcomes = %+v", got)
	}
	// Seeding never overrides a baseline that is already there.
	c.Seed("o/r", 1, 1, t0.Add(time.Hour))
	if got := c.Outcomes(); got.MergedPRs != 4 || got.ClosedIssues != 9 {
		t.Fatalf("Seed overrode a ready baseline: %+v", got)
	}
}

func TestCountsHourlyReconcileOverwrites(t *testing.T) {
	c, h, api := newCountsHarness(t)
	api.set("o/r", 10, 20)
	if !c.ReconcileDue() {
		t.Fatal("the first reconcile is due immediately")
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if c.ReconcileDue() {
		t.Fatal("not due right after a reconcile")
	}

	// Webhooks drift the counters; the next reconcile is the truth.
	c.NoteClosed("o/r", KindPR, true, "hive-bot[bot]")
	c.NoteClosed("o/r", KindPR, true, "hive-bot[bot]")
	c.NoteClosed("o/r", KindIssue, false, "hive-bot[bot]")
	if got := c.Outcomes(); got.MergedPRs != 12 || got.ClosedIssues != 21 {
		t.Fatalf("after events = %d/%d, want 12/21", got.MergedPRs, got.ClosedIssues)
	}

	h.clock.Advance(CountsReconcileInterval - time.Second)
	if c.ReconcileDue() {
		t.Fatal("not due before the interval elapses")
	}
	h.clock.Advance(time.Second)
	if !c.ReconcileDue() {
		t.Fatal("due once the interval elapses")
	}
	api.set("o/r", 11, 25)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := c.Outcomes()
	if got.MergedPRs != 11 || got.ClosedIssues != 25 {
		t.Fatalf("after reconcile = %d/%d, want the API's 11/25", got.MergedPRs, got.ClosedIssues)
	}
	if got.Source != SourceReconcile || !got.LastGoodAt.Equal(h.clock.Now()) {
		t.Fatalf("outcomes = %+v, want source reconcile at now", got)
	}
	if api.callCount() != 2 {
		t.Fatalf("API calls = %d, want 2", api.callCount())
	}
}

func TestCountsReconcile403KeepsLastGoodAndStaleSince(t *testing.T) {
	c, h, api := newCountsHarness(t)
	api.set("o/r", 10, 20)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c.NoteClosed("o/r", KindPR, true, "hive-bot[bot]")

	h.clock.Advance(CountsReconcileInterval)
	api.fail(errors.New("403 You have exceeded a secondary rate limit"))
	if err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("Reconcile must report the failure")
	}
	staleAt := h.clock.Now()
	got := c.Outcomes()
	if got.MergedPRs != 11 || got.ClosedIssues != 20 {
		t.Fatalf("after 403 = %d/%d, want last good plus events 11/20 (never zero)", got.MergedPRs, got.ClosedIssues)
	}
	if !got.Ready || !got.Stale || !got.StaleSince.Equal(staleAt) || got.StaleReason == "" {
		t.Fatalf("after 403 = %+v, want ready+stale since %v", got, staleAt)
	}
	if !got.LastGoodAt.Equal(t0) {
		t.Fatalf("LastGoodAt = %v, want %v", got.LastGoodAt, t0)
	}

	// Events still count while stale; StaleSince keeps the first failure.
	c.NoteClosed("o/r", KindIssue, false, "hive-bot[bot]")
	h.clock.Advance(CountsReconcileInterval)
	_ = c.Reconcile(context.Background())
	got = c.Outcomes()
	if got.ClosedIssues != 21 || !got.StaleSince.Equal(staleAt) {
		t.Fatalf("second failure = %+v, want 21 closed and stale since %v", got, staleAt)
	}

	// Recovery clears the mark and overwrites with the API's answer.
	api.fail(nil)
	api.set("o/r", 12, 22)
	h.clock.Advance(CountsReconcileInterval)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after recovery: %v", err)
	}
	got = c.Outcomes()
	if got.Stale || !got.StaleSince.IsZero() || got.MergedPRs != 12 || got.ClosedIssues != 22 {
		t.Fatalf("after recovery = %+v", got)
	}
}

func TestCountsOutcomesAggregateAcrossRepos(t *testing.T) {
	api := newOutcomeAPI()
	clock := newClock()
	c := NewCounts(CountsOptions{Repos: []string{"o/a", "o/b"}, Reconcile: api.reconcile, Now: clock.Now})
	api.set("o/a", 1, 2)
	api.set("o/b", 3, 4)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := c.Outcomes(); got.MergedPRs != 4 || got.ClosedIssues != 6 || !got.Ready {
		t.Fatalf("aggregate = %+v", got)
	}
	if got := c.Outcomes("o/a"); got.MergedPRs != 1 || got.ClosedIssues != 2 {
		t.Fatalf("o/a = %+v", got)
	}
	if got := c.Outcomes("o/missing"); got.Ready {
		t.Fatal("an unknown repo is not ready")
	}
	// No author configured: every opener counts.
	if !c.NoteClosed("o/b", KindIssue, false, "alice") {
		t.Fatal("without an Author every login counts")
	}
}

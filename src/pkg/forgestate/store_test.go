package forgestate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestNewRequiresFetcher(t *testing.T) {
	if _, err := New(Options{Repos: []string{"o/r"}}); err == nil {
		t.Fatal("New without a Fetcher must fail")
	}
}

func TestNewDefaultsAndRepos(t *testing.T) {
	s, err := New(Options{
		Repos:                []string{"Org/One", "org/one", " ", "org/two"},
		Fetcher:              newFetcher(),
		ReconcileInterval:    30 * time.Second,
		MinReconcileInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Repos(); len(got) != 2 || got[0] != "Org/One" || got[1] != "org/two" {
		t.Fatalf("Repos = %v, want [Org/One org/two] (deduped, case-insensitive, blanks dropped)", got)
	}
	if s.opts.MinReconcileInterval != 30*time.Second {
		t.Errorf("MinReconcileInterval = %v, want clamped to ReconcileInterval", s.opts.MinReconcileInterval)
	}
	if s.opts.FreshnessBound != config.DefaultForgeStateFreshnessBound ||
		s.opts.DriftThreshold != config.DefaultForgeStateDriftThreshold ||
		s.opts.SearchBudget != config.DefaultForgeStateSearchBudget {
		t.Errorf("defaults not applied: %+v", s.opts)
	}
	if s.opts.Metrics != DefaultCounters() || s.opts.Gate == nil || s.opts.Logger == nil || s.opts.After == nil {
		t.Error("Metrics/Gate/Logger/After defaults not applied")
	}
	if s.Ready() {
		t.Error("Ready before any reconcile")
	}
	if _, ok := s.Snapshot("ORG/ONE"); !ok {
		t.Error("Snapshot lookup must be case-insensitive")
	}
	if _, ok := s.Snapshot("nope/nope"); ok {
		t.Error("Snapshot of unknown repo must report false")
	}
	if s.Deliveries("nope/nope") != nil {
		t.Error("Deliveries of unknown repo must be nil")
	}
	if s.Fresh("nope/nope", time.Hour) {
		t.Error("Fresh of unknown repo must be false")
	}
	if _, ok := s.Issue("nope/nope", 1); ok {
		t.Error("Issue of unknown repo must report false")
	}
}

func TestNewWithNoReposIsReady(t *testing.T) {
	s, err := New(Options{Fetcher: newFetcher()})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ready() {
		t.Fatal("a store with no repos has nothing to wait for")
	}
	if err := s.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	o := OptionsFromConfig(config.ForgeStateConfig{ReconcileIntervalS: 120, FreshnessBoundS: 10, DriftThreshold: 0.5, SearchBudget: 7})
	if o.ReconcileInterval != 2*time.Minute || o.FreshnessBound != 10*time.Second || o.DriftThreshold != 0.5 || o.SearchBudget != 7 {
		t.Fatalf("OptionsFromConfig = %+v", o)
	}
	d := OptionsFromConfig(config.ForgeStateConfig{})
	if d.ReconcileInterval != 15*time.Minute || d.FreshnessBound != time.Minute {
		t.Fatalf("OptionsFromConfig defaults = %+v", d)
	}
}

func TestReconcileFillsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList(
		[]Item{issue(1, t0, "bug"), issue(2, t0), closedIssue(3, t0), {Number: 0, State: "open"}},
		[]Item{pr(10, t0, "abc")}, "")
	mustReconcile(t, h, "o/r")

	snap, ok := h.store.Snapshot("o/r")
	if !ok {
		t.Fatal("no snapshot")
	}
	if snap.IssueCount() != 2 || snap.PRCount() != 1 {
		t.Fatalf("counts = %d issues %d prs, want 2/1 (closed and numberless items dropped)", snap.IssueCount(), snap.PRCount())
	}
	if !snap.Ready || snap.Stale || !snap.LastGoodAt.Equal(t0) {
		t.Fatalf("snapshot flags = %+v", snap)
	}
	it := snap.Issues[1]
	if it.Source != SourceReconcile || !it.FetchedAt.Equal(t0) || it.Kind != KindIssue || !it.HasLabel("BUG") {
		t.Fatalf("issue 1 = %+v", it)
	}
	if p, ok := h.store.PR("o/r", 10); !ok || p.HeadSHA != "abc" || p.Kind != KindPR {
		t.Fatalf("PR 10 = %+v, %v", p, ok)
	}
	// Snapshots are copies.
	snap.Issues[1].Labels[0] = "mutated"
	if got, _ := h.store.Issue("o/r", 1); got.Labels[0] != "bug" {
		t.Fatal("Snapshot must deep-copy items")
	}
	if !h.store.Ready() || !h.store.Fresh("o/r", 0) {
		t.Fatal("store must be ready and fresh right after its first reconcile")
	}
	if got := h.metrics.Value(MetricReconcile, "repo", "o/r", "result", resultOK); got != 1 {
		t.Errorf("reconcile ok metric = %d, want 1", got)
	}
}

func TestReconcileUnknownRepo(t *testing.T) {
	h := newHarness(t)
	if err := h.store.Reconcile(context.Background(), "x/y"); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("err = %v, want ErrUnknownRepo", err)
	}
}

// Acceptance: 403 mid-reconcile → previous snapshot intact, stale set,
// never zero counts.
func TestReconcileErrorKeepsLastGoodAndMarksStale(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, []Item{pr(10, t0, "a")}, "")
	mustReconcile(t, h, "o/r")

	h.clock.Advance(time.Minute)
	h.fetcher.failList(errors.New("403 Resource not accessible by integration"))
	if err := h.store.Reconcile(context.Background(), "o/r"); err == nil {
		t.Fatal("Reconcile must report the failure")
	}
	snap, _ := h.store.Snapshot("o/r")
	if snap.IssueCount() != 2 || snap.PRCount() != 1 {
		t.Fatalf("counts after 403 = %d/%d, want last good 2/1", snap.IssueCount(), snap.PRCount())
	}
	if !snap.Stale || !snap.StaleSince.Equal(t0.Add(time.Minute)) || !strings.Contains(snap.StaleReason, "403") {
		t.Fatalf("stale flags = %+v", snap)
	}
	if !snap.LastGoodAt.Equal(t0) {
		t.Fatalf("LastGoodAt = %v, want the last good reconcile", snap.LastGoodAt)
	}
	if h.store.Fresh("o/r", time.Hour) {
		t.Fatal("a stale snapshot is never fresh")
	}
	if got := h.metrics.Value(MetricStale, "repo", "o/r"); got != 1 {
		t.Errorf("stale gauge = %d, want 1", got)
	}

	// StaleSince marks the start of the failures, not the latest one.
	h.clock.Advance(time.Minute)
	_ = h.store.Reconcile(context.Background(), "o/r")
	if snap, _ := h.store.Snapshot("o/r"); !snap.StaleSince.Equal(t0.Add(time.Minute)) {
		t.Fatalf("StaleSince moved to %v", snap.StaleSince)
	}

	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, []Item{pr(10, t0, "a")}, "")
	mustReconcile(t, h, "o/r")
	if snap, _ := h.store.Snapshot("o/r"); snap.Stale || !snap.StaleSince.IsZero() || snap.StaleReason != "" {
		t.Fatalf("success must clear stale: %+v", snap)
	}
	if got := h.metrics.Value(MetricStale, "repo", "o/r"); got != 0 {
		t.Errorf("stale gauge = %d, want 0", got)
	}
}

func TestReconcileBudgetDenialMarksStale(t *testing.T) {
	h := newHarness(t)
	h.store.opts.Gate = denyGate{}
	if err := h.store.Reconcile(context.Background(), "o/r"); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if l, _ := h.fetcher.calls(); l != 0 {
		t.Fatal("a refused reconcile must not reach the fetcher")
	}
	snap, _ := h.store.Snapshot("o/r")
	if !snap.Stale || snap.Ready {
		t.Fatalf("snapshot = %+v, want stale and not ready", snap)
	}
	if got := h.metrics.Value(MetricReconcile, "repo", "o/r", "result", resultDenied); got != 1 {
		t.Errorf("denied metric = %d", got)
	}
}

type denyGate struct{}

func (denyGate) Admit(context.Context, Bucket) error { return ErrBudgetExhausted }

func TestReconcileNotModifiedKeepsCache(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0)}, nil, `"e1"`)
	mustReconcile(t, h, "o/r")
	h.clock.Advance(time.Minute)
	mustReconcile(t, h, "o/r")
	if got := h.fetcher.etags; len(got) != 2 || got[0] != "" || got[1] != `"e1"` {
		t.Fatalf("etags sent = %q, want the cached one on the second list", got)
	}
	snap, _ := h.store.Snapshot("o/r")
	if snap.IssueCount() != 1 || !snap.LastGoodAt.Equal(t0.Add(time.Minute)) || snap.Drift != 0 {
		t.Fatalf("snapshot after 304 = %+v", snap)
	}
	if got := h.metrics.Value(MetricReconcile, "repo", "o/r", "result", resultNotModified); got != 1 {
		t.Errorf("not_modified metric = %d", got)
	}
}

type notModifiedFetcher struct{ *fakeFetcher }

func (notModifiedFetcher) List(context.Context, string, string) (ListResult, error) {
	return ListResult{NotModified: true}, nil
}

func TestReconcileNotModifiedWithoutETagIsAnError(t *testing.T) {
	h := newHarness(t)
	h.store.opts.Fetcher = notModifiedFetcher{h.fetcher}
	if err := h.store.Reconcile(context.Background(), "o/r"); err == nil {
		t.Fatal("a 304 with no cached etag has no baseline and must fail")
	}
	if snap, _ := h.store.Snapshot("o/r"); !snap.Stale || snap.Ready {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// Acceptance: startup — full reconcile of every repo completes before the
// first governor decision pass.
func TestWaitReadyBlocksUntilEveryRepoReconciled(t *testing.T) {
	h := newHarness(t, "o/a", "o/b")
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")

	done := make(chan error, 1)
	go func() { done <- h.store.WaitReady(context.Background()) }()

	h.fetcher.failList(errors.New("boom"))
	_ = h.store.Reconcile(context.Background(), "o/a")
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/a")
	select {
	case err := <-done:
		t.Fatalf("WaitReady returned (%v) with o/b never reconciled", err)
	default:
	}
	if h.store.Ready() {
		t.Fatal("Ready with o/b never reconciled")
	}

	mustReconcile(t, h, "o/b")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitReady did not resolve after every repo reconciled")
	}
	mustReconcile(t, h, "o/b") // a second success must not re-close the gate
}

func TestWaitReadyHonoursContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.store.WaitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Acceptance: drift between cache and API is computed and recorded.
func TestReconcileRecordsDrift(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0), issue(3, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	if got := h.metrics.Value(MetricDrift, "repo", "o/r"); got != 0 {
		t.Fatalf("first reconcile drift = %d, want 0 (no baseline)", got)
	}

	// Missed webhooks: #1 relabelled, #2 closed, #4 opened.
	h.clock.Advance(time.Minute)
	h.fetcher.setList([]Item{issue(1, t0.Add(time.Second), "x"), issue(3, t0), issue(4, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	if got := h.metrics.Value(MetricDrift, "repo", "o/r"); got != 3 {
		t.Fatalf("drift = %d, want 3 (changed, missing, extra)", got)
	}
	snap, _ := h.store.Snapshot("o/r")
	if snap.Drift != 3 || !snap.DriftHigh {
		t.Fatalf("snapshot drift = %d high=%v", snap.Drift, snap.DriftHigh)
	}
	if got := h.store.interval(h.store.lookup("o/r")); got != h.store.opts.MinReconcileInterval {
		t.Fatalf("interval while drifting = %v, want the short one", got)
	}

	h.clock.Advance(time.Minute)
	mustReconcile(t, h, "o/r")
	if snap, _ := h.store.Snapshot("o/r"); snap.Drift != 0 || snap.DriftHigh {
		t.Fatalf("drift after an agreeing reconcile = %d high=%v", snap.Drift, snap.DriftHigh)
	}
	if got := h.store.interval(h.store.lookup("o/r")); got != h.store.opts.ReconcileInterval {
		t.Fatalf("interval = %v, want the configured one", got)
	}
}

// Patches that land while a list is in flight are newer than the list.
func TestReconcileKeepsPatchesAppliedDuringList(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, nil, "")
	mustReconcile(t, h, "o/r")

	h.clock.Advance(time.Minute)
	h.fetcher.onList = func() {
		h.clock.Advance(time.Second)
		if err := h.store.WriteThrough("o/r", Mutation{Op: OpLabelAdd, Kind: KindIssue, Number: 1, Labels: []string{"stage/merge"}}); err != nil {
			t.Errorf("WriteThrough: %v", err)
		}
		if err := h.store.WriteThrough("o/r", Mutation{Op: OpClose, Kind: KindIssue, Number: 2}); err != nil {
			t.Errorf("WriteThrough: %v", err)
		}
	}
	mustReconcile(t, h, "o/r")
	h.fetcher.onList = nil

	if it, ok := h.store.Issue("o/r", 1); !ok || !it.HasLabel("stage/merge") || it.Source != SourceWriteThrough {
		t.Fatalf("issue 1 = %+v %v, want the in-flight write-through to survive", it, ok)
	}
	if _, ok := h.store.Issue("o/r", 2); ok {
		t.Fatal("issue 2 closed during the list must stay removed")
	}

	// The next reconcile that starts after the patches is authoritative.
	h.clock.Advance(time.Minute)
	h.fetcher.setList([]Item{issue(1, t0.Add(2*time.Minute), "stage/merge")}, nil, "")
	mustReconcile(t, h, "o/r")
	if it, _ := h.store.Issue("o/r", 1); it.Source != SourceReconcile {
		t.Fatalf("issue 1 source = %s, want reconcile", it.Source)
	}
	if n := len(h.store.lookup("o/r").tombs); n != 0 {
		t.Fatalf("%d tombstones survive a later reconcile", n)
	}
}

// Acceptance: irreversible actions read through with a freshness bound and
// never act on stale snapshots.
func TestFreshnessBound(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.store.ReadThrough(context.Background(), "o/r", KindIssue, 1, 0); !errors.Is(err, ErrNotReady) {
		t.Fatalf("ReadThrough before ready = %v, want ErrNotReady", err)
	}
	h.fetcher.setList([]Item{issue(1, t0)}, []Item{pr(10, t0, "a")}, "")
	mustReconcile(t, h, "o/r")

	if !h.store.Fresh("o/r", 0) {
		t.Fatal("fresh right after reconcile")
	}
	h.clock.Advance(61 * time.Second)
	if h.store.Fresh("o/r", 0) {
		t.Fatal("Fresh must reject a snapshot older than the default 60s bound")
	}
	if !h.store.Fresh("o/r", 2*time.Minute) {
		t.Fatal("Fresh within an explicit wider bound")
	}

	// Within bound: served from cache, no API call.
	if it, ok, err := h.store.ReadThrough(context.Background(), "o/r", KindIssue, 1, 2*time.Minute); err != nil || !ok || it.Number != 1 {
		t.Fatalf("cached ReadThrough = %+v %v %v", it, ok, err)
	}
	if _, n := h.fetcher.calls(); n != 0 {
		t.Fatalf("cached ReadThrough made %d calls", n)
	}

	// Past bound: re-fetched.
	h.fetcher.setItem(Item{Kind: KindPR, Number: 10, State: "open", UpdatedAt: t0.Add(time.Minute), HeadSHA: "b"})
	it, ok, err := h.store.ReadThrough(context.Background(), "o/r", KindPR, 10, 0)
	if err != nil || !ok || it.HeadSHA != "b" || it.Source != SourceReadThrough {
		t.Fatalf("ReadThrough PR = %+v %v %v", it, ok, err)
	}

	// A merged PR reads as not actionable and leaves the cache.
	h.clock.Advance(2 * time.Minute)
	h.fetcher.setItem(Item{Kind: KindPR, Number: 10, State: "closed", Merged: true, UpdatedAt: t0.Add(2 * time.Minute)})
	if _, ok, err := h.store.ReadThrough(context.Background(), "o/r", KindPR, 10, 0); err != nil || ok {
		t.Fatalf("merged PR ReadThrough ok=%v err=%v, want ok=false", ok, err)
	}
	if _, ok := h.store.PR("o/r", 10); ok {
		t.Fatal("merged PR still cached")
	}

	// A deleted issue reads as gone.
	h.fetcher.mu.Lock()
	delete(h.fetcher.items, itemKey{KindIssue, 1})
	h.fetcher.mu.Unlock()
	if _, ok, err := h.store.ReadThrough(context.Background(), "o/r", KindIssue, 1, 0); err != nil || ok {
		t.Fatalf("deleted issue ReadThrough ok=%v err=%v", ok, err)
	}
	if _, ok := h.store.Issue("o/r", 1); ok {
		t.Fatal("deleted issue still cached")
	}

	// A failed read is an error, never a stale answer.
	h.fetcher.failItems(errors.New("502"))
	if _, _, err := h.store.ReadThrough(context.Background(), "o/r", KindIssue, 5, 0); err == nil {
		t.Fatal("ReadThrough must surface a fetch error")
	}
	if got := h.metrics.Value(MetricReadThrough, "repo", "o/r", "result", resultError); got != 1 {
		t.Errorf("readthrough error metric = %d", got)
	}

	// Stale: refuse outright.
	h.fetcher.failList(errors.New("403"))
	_ = h.store.Reconcile(context.Background(), "o/r")
	if _, _, err := h.store.ReadThrough(context.Background(), "o/r", KindIssue, 1, time.Hour); !errors.Is(err, ErrStale) {
		t.Fatalf("ReadThrough on stale = %v, want ErrStale", err)
	}
	if _, _, err := h.store.ReadThrough(context.Background(), "x/y", KindIssue, 1, 0); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("ReadThrough unknown repo = %v", err)
	}
}

// Acceptance: agent mutation → visible in cache before the next read.
func TestWriteThroughVisibleBeforeNextRead(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0, "stage/triage")}, []Item{pr(10, t0, "a")}, "")
	mustReconcile(t, h, "o/r")
	h.clock.Advance(time.Second)

	steps := []struct {
		m     Mutation
		check func(Item, bool) bool
	}{
		{Mutation{Op: OpLabelAdd, Kind: KindIssue, Number: 1, Labels: []string{"stage/ready", "", "STAGE/READY"}},
			func(it Item, ok bool) bool { return ok && it.HasLabel("stage/ready") && len(it.Labels) == 2 }},
		{Mutation{Op: OpLabelRemove, Kind: KindIssue, Number: 1, Labels: []string{"Stage/Triage"}},
			func(it Item, ok bool) bool { return ok && !it.HasLabel("stage/triage") && len(it.Labels) == 1 }},
		{Mutation{Op: OpClaim, Kind: KindIssue, Number: 1, Labels: []string{"hive/claimed"}, Assignee: "hive-bot"},
			func(it Item, ok bool) bool { return ok && it.HasLabel("hive/claimed") && len(it.Assignees) == 1 }},
		{Mutation{Op: OpClaim, Kind: KindIssue, Number: 1},
			func(it Item, ok bool) bool { return ok && len(it.Assignees) == 1 }},
		{Mutation{Op: OpComment, Kind: KindIssue, Number: 1},
			func(it Item, ok bool) bool {
				return ok && it.Source == SourceWriteThrough && it.FetchedAt.Equal(t0.Add(time.Second))
			}},
	}
	for _, st := range steps {
		if err := h.store.WriteThrough("o/r", st.m); err != nil {
			t.Fatalf("WriteThrough(%+v): %v", st.m, err)
		}
		if it, ok := h.store.Issue("o/r", 1); !st.check(it, ok) {
			t.Fatalf("after %s: issue = %+v %v", st.m.Op, it, ok)
		}
	}

	// Merge removes at once; a lagging read of the pre-merge PR cannot
	// resurrect it.
	if err := h.store.WriteThrough("o/r", Mutation{Op: OpMerge, Kind: KindPR, Number: 10}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.PR("o/r", 10); ok {
		t.Fatal("merged PR still cached")
	}
	if err := h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "d1", Kind: KindPR, Number: 10, Action: "synchronize"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.PR("o/r", 10); ok {
		t.Fatal("a stale replica read resurrected a merged PR")
	}

	// Close of an uncached issue still tombstones; reopen clears it so the
	// next read re-adds the item.
	if err := h.store.WriteThrough("o/r", Mutation{Op: OpClose, Kind: KindIssue, Number: 7}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.lookup("o/r").tombs[itemKey{KindIssue, 7}]; !ok {
		t.Fatal("close of an uncached item must leave a tombstone")
	}
	if err := h.store.WriteThrough("o/r", Mutation{Op: OpReopen, Kind: KindIssue, Number: 7}); err != nil {
		t.Fatal(err)
	}
	h.fetcher.setItem(issue(7, t0))
	if err := h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "d2", Kind: KindIssue, Number: 7, Action: "reopened"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Issue("o/r", 7); !ok {
		t.Fatal("reopened issue not re-added")
	}

	// Mutations on items the cache does not hold are no-ops.
	if err := h.store.WriteThrough("o/r", Mutation{Op: OpLabelAdd, Kind: KindIssue, Number: 99, Labels: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Issue("o/r", 99); ok {
		t.Fatal("label add invented an item")
	}
	if got := h.metrics.Value(MetricWriteThrough, "repo", "o/r", "op", resultMiss); got != 1 {
		t.Errorf("miss metric = %d", got)
	}
	if got := h.metrics.Value(MetricWriteThrough, "repo", "o/r", "op", string(OpLabelAdd)); got != 1 {
		t.Errorf("label_add metric = %d", got)
	}
}

func TestWriteThroughRejectsBadMutations(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		repo string
		m    Mutation
	}{
		{"x/y", Mutation{Op: OpComment, Kind: KindIssue, Number: 1}},
		{"o/r", Mutation{Op: OpComment, Kind: KindIssue}},
		{"o/r", Mutation{Op: OpComment, Kind: "wiki", Number: 1}},
		{"o/r", Mutation{Op: "explode", Kind: KindIssue, Number: 1}},
	}
	for _, c := range cases {
		if err := h.store.WriteThrough(c.repo, c.m); err == nil {
			t.Errorf("WriteThrough(%s, %+v) = nil, want error", c.repo, c.m)
		}
	}
}

func TestRunReconcilesThenReschedules(t *testing.T) {
	h := newHarness(t, "o/a", "o/b")
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	waits := make(chan time.Duration, 8)
	fire := make(chan time.Time)
	h.store.opts.After = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.store.Run(ctx) }()

	next := func() time.Duration {
		t.Helper()
		select {
		case d := <-waits:
			return d
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not reach its wait")
			return 0
		}
	}
	if d := next(); d != h.store.opts.ReconcileInterval {
		t.Fatalf("first wait = %v, want %v", d, h.store.opts.ReconcileInterval)
	}
	if !h.store.Ready() {
		t.Fatal("Run's first pass must reconcile every repo")
	}

	// Nothing due yet: a spurious wake reconciles nothing.
	fire <- t0
	if d := next(); d != h.store.opts.ReconcileInterval {
		t.Fatalf("wait after spurious wake = %v", d)
	}
	if l, _ := h.fetcher.calls(); l != 2 {
		t.Fatalf("list calls = %d, want 2", l)
	}

	// Due and failing: retried on the short interval.
	h.fetcher.failList(errors.New("403"))
	h.clock.Advance(h.store.opts.ReconcileInterval)
	fire <- t0
	if d := next(); d != h.store.opts.MinReconcileInterval {
		t.Fatalf("wait while stale = %v, want %v", d, h.store.opts.MinReconcileInterval)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.store.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestItemHelpers(t *testing.T) {
	if (Item{State: "open"}).Open() != true || (Item{State: "CLOSED"}).Open() || (Item{Merged: true}).Open() || (Item{Closed: true}).Open() {
		t.Fatal("Open")
	}
	if sameSet([]string{"a"}, []string{"a", "b"}) || !sameSet([]string{"B", "a"}, []string{"a", "b"}) || sameSet([]string{"a"}, []string{"c"}) {
		t.Fatal("sameSet")
	}
	if (Item{}).HasLabel("x") {
		t.Fatal("HasLabel")
	}
}

func TestSupersedes(t *testing.T) {
	cur := pr(1, t0, "a")
	if supersedes(pr(1, t0.Add(-time.Second), "z"), cur) {
		t.Error("older never supersedes")
	}
	if !supersedes(pr(1, t0.Add(time.Second), "a"), cur) {
		t.Error("newer supersedes")
	}
	if !supersedes(pr(1, t0, "b"), cur) {
		t.Error("same updated_at with a new head SHA supersedes")
	}
	if supersedes(pr(1, t0, ""), cur) || supersedes(pr(1, t0, "a"), cur) {
		t.Error("same updated_at and no new head SHA does not supersede")
	}
	if supersedes(issue(1, t0), issue(1, t0)) {
		t.Error("issues compare by updated_at only")
	}
}

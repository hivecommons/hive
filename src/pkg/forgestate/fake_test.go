package forgestate

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeFetcher is an in-memory GitHub. Single-item reads pop from a per-item
// queue first (to script out-of-order replica answers), then fall back to
// the current item map; a missing item is ErrNotFound.
type fakeFetcher struct {
	mu        sync.Mutex
	list      ListResult
	listErr   error
	items     map[itemKey]Item
	queue     map[itemKey][]Item
	itemErr   error
	listCalls int
	itemCalls int
	etags     []string
	onList    func()
}

func newFetcher() *fakeFetcher {
	return &fakeFetcher{items: map[itemKey]Item{}, queue: map[itemKey][]Item{}}
}

func (f *fakeFetcher) setList(issues, prs []Item, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = ListResult{Issues: issues, PRs: prs, ETag: etag}
	f.listErr = nil
	for _, it := range issues {
		it.Kind = KindIssue
		f.items[itemKey{KindIssue, it.Number}] = it
	}
	for _, it := range prs {
		it.Kind = KindPR
		f.items[itemKey{KindPR, it.Number}] = it
	}
}

func (f *fakeFetcher) setItem(it Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[itemKey{it.Kind, it.Number}] = it
}

func (f *fakeFetcher) enqueue(items ...Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		k := itemKey{it.Kind, it.Number}
		f.queue[k] = append(f.queue[k], it)
	}
}

func (f *fakeFetcher) failList(err error) {
	f.mu.Lock()
	f.listErr = err
	f.mu.Unlock()
}

func (f *fakeFetcher) failItems(err error) {
	f.mu.Lock()
	f.itemErr = err
	f.mu.Unlock()
}

func (f *fakeFetcher) calls() (list, item int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.itemCalls
}

func (f *fakeFetcher) List(_ context.Context, _ string, etag string) (ListResult, error) {
	f.mu.Lock()
	f.listCalls++
	f.etags = append(f.etags, etag)
	hook := f.onList
	res, err := f.list, f.listErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return ListResult{}, err
	}
	if etag != "" && etag == res.ETag {
		return ListResult{ETag: etag, NotModified: true}, nil
	}
	return res, nil
}

func (f *fakeFetcher) get(k itemKey) (Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.itemCalls++
	if f.itemErr != nil {
		return Item{}, f.itemErr
	}
	if q := f.queue[k]; len(q) > 0 {
		f.queue[k] = q[1:]
		return q[0], nil
	}
	if it, ok := f.items[k]; ok {
		return it, nil
	}
	return Item{}, ErrNotFound
}

func (f *fakeFetcher) Issue(_ context.Context, _ string, n int) (Item, error) {
	return f.get(itemKey{KindIssue, n})
}

func (f *fakeFetcher) PR(_ context.Context, _ string, n int) (Item, error) {
	return f.get(itemKey{KindPR, n})
}

type harness struct {
	store   *Store
	fetcher *fakeFetcher
	clock   *fakeClock
	metrics *Counters
}

func newHarness(t *testing.T, repos ...string) *harness {
	t.Helper()
	if len(repos) == 0 {
		repos = []string{"o/r"}
	}
	h := &harness{fetcher: newFetcher(), clock: newClock(), metrics: NewCounters()}
	s, err := New(Options{
		Repos:   repos,
		Fetcher: h.fetcher,
		Metrics: h.metrics,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     h.clock.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.store = s
	return h
}

func issue(n int, updated time.Time, labels ...string) Item {
	return Item{Kind: KindIssue, Number: n, State: "open", UpdatedAt: updated, Labels: labels}
}

func closedIssue(n int, updated time.Time) Item {
	return Item{Kind: KindIssue, Number: n, State: "closed", Closed: true, UpdatedAt: updated}
}

func pr(n int, updated time.Time, sha string) Item {
	return Item{Kind: KindPR, Number: n, State: "open", UpdatedAt: updated, HeadSHA: sha}
}

func mustReconcile(t *testing.T, h *harness, repo string) {
	t.Helper()
	if err := h.store.Reconcile(context.Background(), repo); err != nil {
		t.Fatalf("Reconcile(%s): %v", repo, err)
	}
}

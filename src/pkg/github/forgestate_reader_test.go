package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// forgeTestForge is a one-repo fake GitHub that counts list calls and
// single-item GETs separately, so a test can assert that a governor pass
// served from the state cache lists nothing.
type forgeTestForge struct {
	t      *testing.T
	org    string
	repo   string
	mu     sync.Mutex
	issues []wireIssue
	prs    []wirePR
	// failing makes the two list endpoints answer 503.
	failing atomic.Bool
	// listHits counts open-state lists (the governor's work listing);
	// closedListHits the attributed closed-PR history scan.
	listHits       atomic.Int64
	closedListHits atomic.Int64
	issueGets      atomic.Int64
	prGets         atomic.Int64
	labelAdds      atomic.Int64
	srv            *httptest.Server
	client         *Client
	store          *forgestate.Store
	now            func() time.Time
}

func newForgeTestForge(t *testing.T) *forgeTestForge {
	t.Helper()
	f := &forgeTestForge{t: t, org: "org", repo: "cached"}
	f.issues = []wireIssue{
		{Number: 1, Title: "first", User: wireUser{"alice"}, CreatedAt: hoursAgo(3), State: "open"},
		{Number: 2, Title: "second", User: wireUser{"bob"}, CreatedAt: hoursAgo(2), State: "open"},
	}
	f.prs = []wirePR{{Number: 10, Title: "a pr", User: wireUser{"carol"}, CreatedAt: hoursAgo(1)}}
	mux := http.NewServeMux()
	base := fmt.Sprintf("/repos/%s/%s", f.org, f.repo)
	fail := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"message":"unavailable"}`)
	}
	mux.HandleFunc(base+"/issues", func(w http.ResponseWriter, r *http.Request) {
		f.listHits.Add(1)
		if f.failing.Load() {
			fail(w)
			return
		}
		f.mu.Lock()
		body := mustMarshal(t, f.issues)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	mux.HandleFunc(base+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") == "closed" {
			f.closedListHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		f.listHits.Add(1)
		if f.failing.Load() {
			fail(w)
			return
		}
		f.mu.Lock()
		body := mustMarshal(t, f.prs)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	mux.HandleFunc(base+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, base+"/issues/")
		if strings.HasSuffix(rest, "/labels") && r.Method == http.MethodPost {
			f.labelAdds.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		if r.Method != http.MethodGet || strings.Contains(rest, "/") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		f.issueGets.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, is := range f.issues {
			if fmt.Sprint(is.Number) == rest {
				w.Header().Set("Content-Type", "application/json")
				w.Write(mustMarshal(t, is))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc(base+"/pulls/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, base+"/pulls/")
		if strings.Contains(rest, "/") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[]`))
			return
		}
		f.prGets.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, pr := range f.prs {
			if fmt.Sprint(pr.Number) == rest {
				w.Header().Set("Content-Type", "application/json")
				w.Write(mustMarshal(t, pr))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.client = newTestClient(t, f.srv, f.org, []string{f.repo})
	f.now = time.Now
	store, err := forgestate.New(forgestate.Options{
		Repos:   f.client.Repositories(),
		Fetcher: ForgeStateFetcher{Current: func() *Client { return f.client }},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return f.now() },
	})
	if err != nil {
		t.Fatalf("forgestate.New: %v", err)
	}
	f.store = store
	f.client.SetForgeState(store)
	return f
}

func (f *forgeTestForge) reconcile() error {
	return f.store.Reconcile(context.Background(), f.repo)
}

func (f *forgeTestForge) mustReconcile() {
	f.t.Helper()
	if err := f.reconcile(); err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
}

func (f *forgeTestForge) resetCounters() {
	f.listHits.Store(0)
	f.issueGets.Store(0)
	f.prGets.Store(0)
}

func issueNumbers(res *ActionableResult) []int {
	var out []int
	for _, is := range res.Issues.Items {
		out = append(out, is.Number)
	}
	return out
}

// A governor pass with the state cache attached does not list GitHub: the
// reconcile is the only full list, and a wake between reconciles plans from
// the cached snapshot (#13375).
func TestEnumerateActionable_ForgeStateServesWithoutListing(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	if got := f.listHits.Load(); got != 2 {
		t.Fatalf("reconcile list hits = %d, want 2 (issues + pulls)", got)
	}
	f.resetCounters()

	for pass := 0; pass < 3; pass++ {
		res, err := f.client.EnumerateActionable(context.Background())
		if err != nil {
			t.Fatalf("pass %d: EnumerateActionable: %v", pass, err)
		}
		if got := issueNumbers(res); len(got) != 2 {
			t.Fatalf("pass %d: issues = %v, want 2 cached issues", pass, got)
		}
		if len(res.PRs.Items) != 1 || res.PRs.Items[0].Number != 10 {
			t.Fatalf("pass %d: PRs = %+v, want cached PR #10", pass, res.PRs.Items)
		}
		if len(res.StaleRepos) != 0 {
			t.Fatalf("pass %d: StaleRepos = %v, want none after a good reconcile", pass, res.StaleRepos)
		}
		if _, ok := res.ItemPatches[ItemPatchKey(f.repo, 1, false)]; !ok {
			t.Fatalf("pass %d: ItemPatches lacks the cached issue's fetch time: %v", pass, res.ItemPatches)
		}
	}
	if got := f.listHits.Load(); got != 0 {
		t.Fatalf("open-work list hits during cached passes = %d, want 0", got)
	}
	// The attributed closed-PR history scan is not part of the cache: it
	// runs once and is reused for the reconcile interval after that.
	if got := f.closedListHits.Load(); got != 1 {
		t.Fatalf("closed-PR scans across 3 cached passes = %d, want 1", got)
	}
}

// Reconcile is the writer of record: a re-list replaces the cached listing,
// and the next pass sees the new population.
func TestEnumerateActionable_ForgeStateReconcileOverwrites(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	f.mu.Lock()
	f.issues = []wireIssue{{Number: 3, Title: "third", User: wireUser{"dave"}, CreatedAt: hoursAgo(1), State: "open"}}
	f.prs = nil
	f.mu.Unlock()

	res, err := f.client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable before reconcile: %v", err)
	}
	if got := issueNumbers(res); len(got) != 2 {
		t.Fatalf("before reconcile issues = %v, want the 2 cached ones", got)
	}
	// Let the clock move so the re-list's items win over the cached ones.
	later := time.Now().Add(time.Second)
	f.now = func() time.Time { return later }
	f.mustReconcile()
	res, err = f.client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable after reconcile: %v", err)
	}
	if got := issueNumbers(res); len(got) != 1 || got[0] != 3 {
		t.Fatalf("after reconcile issues = %v, want [3]", got)
	}
	if len(res.PRs.Items) != 0 {
		t.Fatalf("after reconcile PRs = %+v, want none", res.PRs.Items)
	}
}

// A failed reconcile leaves the last good listing in place but marks the
// repo stale: the pass still plans from it (not a drop in work) and the
// irreversible guard refuses a close or merge on it.
func TestEnumerateActionable_ForgeStateStaleSnapshotBlocksIrreversible(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	f.failing.Store(true)
	if err := f.reconcile(); err == nil {
		t.Fatal("Reconcile against a 503 list succeeded; want an error")
	}
	f.failing.Store(false)

	res, err := f.client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if got := issueNumbers(res); len(got) != 2 {
		t.Fatalf("stale pass issues = %v, want the 2 last-good issues", got)
	}
	if !res.RepoStale(f.repo) {
		t.Fatalf("StaleRepos = %v, want %q marked stale", res.StaleRepos, f.repo)
	}
	if res.StaleSince.IsZero() {
		t.Fatal("StaleSince is zero; want the last good reconcile time")
	}
	guard := f.client.SnapshotFreshnessGuard(res, time.Now)
	err = guard(context.Background(), f.org+"/"+f.repo, 1, false)
	if !errors.Is(err, ErrSnapshotStaleForDecision) {
		t.Fatalf("guard on stale repo = %v, want ErrSnapshotStaleForDecision", err)
	}
	ctx := WithIrreversibleGuard(context.Background(), guard)
	if err := f.client.CloseIssue(ctx, f.repo, 1, IssueCloseOptions{}); !errors.Is(err, ErrSnapshotStaleForDecision) {
		t.Fatalf("CloseIssue on stale snapshot = %v, want ErrSnapshotStaleForDecision", err)
	}
	if got := f.issueGets.Load(); got != 0 {
		t.Fatalf("issue GETs = %d; a stale repo must refuse without a read", got)
	}
}

// The freshness bound: a cached copy younger than DecisionFreshnessBound
// is acted on without a call; an older one costs exactly one targeted GET,
// which also refreshes the cache.
func TestEnumerateActionable_ForgeStateFreshnessRefresh(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	res, err := f.client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	f.resetCounters()

	fresh := f.client.SnapshotFreshnessGuard(res, time.Now)
	if err := fresh(context.Background(), f.org+"/"+f.repo, 1, false); err != nil {
		t.Fatalf("guard on a fresh copy = %v, want nil", err)
	}
	if got := f.issueGets.Load(); got != 0 {
		t.Fatalf("issue GETs for a fresh copy = %d, want 0", got)
	}

	old := f.client.SnapshotFreshnessGuard(res, func() time.Time { return time.Now().Add(2 * DecisionFreshnessBound) })
	if err := old(context.Background(), f.org+"/"+f.repo, 1, false); err != nil {
		t.Fatalf("guard on an aged copy = %v, want nil after one re-read", err)
	}
	if got := f.issueGets.Load(); got != 1 {
		t.Fatalf("issue GETs for an aged copy = %d, want exactly 1", got)
	}
	if got := f.listHits.Load(); got != 0 {
		t.Fatalf("list hits during a freshness refresh = %d, want 0", got)
	}
	it, ok := f.store.Issue(f.repo, 1)
	if !ok || it.Source != forgestate.SourceReadThrough {
		t.Fatalf("cache item after refresh = %+v (ok=%v), want a read-through observation", it, ok)
	}

	// The confirming read finds the item gone: the action is refused.
	f.mu.Lock()
	f.issues = f.issues[1:]
	f.mu.Unlock()
	err = old(context.Background(), f.org+"/"+f.repo, 1, false)
	if err == nil {
		t.Fatal("guard on a vanished item = nil, want an error")
	}
}

// Before the first successful reconcile the cache says nothing about the
// repo: the pass fails (every repo unavailable) instead of planning from an
// empty listing, and the error is a transient one so last-good substitution
// applies once there is a last-good.
func TestEnumerateActionable_ForgeStateNotReadyIsTransient(t *testing.T) {
	f := newForgeTestForge(t)
	if _, err := f.client.EnumerateActionable(context.Background()); err == nil {
		t.Fatal("EnumerateActionable before the first reconcile succeeded; want an error")
	}
	if got := f.listHits.Load(); got != 0 {
		t.Fatalf("list hits before ready = %d; the governor must not list around the cache", got)
	}
	if !isTransientEnumerationError(fmt.Errorf("wrap: %w", ErrForgeStateNotReady)) {
		t.Fatal("ErrForgeStateNotReady is not classified transient")
	}
}

// A label added through the client is written through to the cache, so the
// next pass sees it without waiting for a webhook or reconcile.
func TestEnumerateActionable_ForgeStateWriteThroughLabel(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	if err := f.client.AddLabels(context.Background(), f.repo, 2, []string{"hive/claimed"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if got := f.labelAdds.Load(); got != 1 {
		t.Fatalf("label POSTs = %d, want 1", got)
	}
	it, ok := f.store.Issue(f.repo, 2)
	if !ok {
		t.Fatal("issue #2 missing from the cache after write-through")
	}
	var found bool
	for _, l := range it.Labels {
		if l == "hive/claimed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cache labels after write-through = %v, want hive/claimed", it.Labels)
	}
	f.resetCounters()
	res, err := f.client.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if got := f.listHits.Load(); got != 0 {
		t.Fatalf("list hits = %d, want 0", got)
	}
	for _, is := range res.Issues.Items {
		if is.Number == 2 {
			for _, l := range is.Labels {
				if l == "hive/claimed" {
					return
				}
			}
			t.Fatalf("issue #2 labels in the pass = %v, want the written-through label", is.Labels)
		}
	}
	// #2 may be classified out of the actionable list by the new label; the
	// cache assertion above is the contract.
}

// A webhook-prompted single-item read patches the cache as a webhook
// observation carrying the delivery id.
func TestFetchWebhookItemSnapshot_ObservesIntoForgeState(t *testing.T) {
	f := newForgeTestForge(t)
	f.mustReconcile()
	f.mu.Lock()
	f.issues[0].Labels = []wireLabel{{Name: "bug"}}
	f.issues[0].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	f.mu.Unlock()
	ctx := WithWebhookDelivery(context.Background(), "delivery-42")
	snap, err := f.client.FetchWebhookItemSnapshot(ctx, f.repo, 1, false)
	if err != nil {
		t.Fatalf("FetchWebhookItemSnapshot: %v", err)
	}
	if snap.DeliveryID != "delivery-42" {
		t.Fatalf("snapshot DeliveryID = %q, want delivery-42", snap.DeliveryID)
	}
	it, ok := f.store.Issue(f.repo, 1)
	if !ok || it.Source != forgestate.SourceWebhook {
		t.Fatalf("cache item = %+v (ok=%v), want a webhook observation", it, ok)
	}
	if len(it.Labels) != 1 || it.Labels[0] != "bug" {
		t.Fatalf("cache labels = %v, want [bug]", it.Labels)
	}
	s, _ := f.store.Snapshot(f.repo)
	if s.LastDeliveryID != "delivery-42" {
		t.Fatalf("LastDeliveryID = %q, want delivery-42", s.LastDeliveryID)
	}
}

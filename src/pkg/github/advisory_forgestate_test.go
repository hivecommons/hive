package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// fakeForgeState is a forgeStateReader with scripted answers.

func useAdvisoryForgeState(t *testing.T, r forgeStateReader) {
	t.Helper()
	advisoryForgeStateForTest.mu.Lock()
	advisoryForgeStateForTest.r = r
	advisoryForgeStateForTest.mu.Unlock()
	t.Cleanup(func() {
		advisoryForgeStateForTest.mu.Lock()
		advisoryForgeStateForTest.r = nil
		advisoryForgeStateForTest.mu.Unlock()
	})
}

type fakeForgeState struct {
	snap      forgestate.Snapshot
	snapOK    bool
	readItem  forgestate.Item
	readOK    bool
	readErr   error
	readCalls int
	mutations []forgestate.Mutation
}

func (f *fakeForgeState) Snapshot(string) (forgestate.Snapshot, bool) { return f.snap, f.snapOK }
func (f *fakeForgeState) ReadThrough(_ context.Context, _ string, _ forgestate.Kind, _ int, _ time.Duration) (forgestate.Item, bool, error) {
	f.readCalls++
	return f.readItem, f.readOK, f.readErr
}
func (f *fakeForgeState) WriteThrough(_ string, m forgestate.Mutation) error {
	f.mutations = append(f.mutations, m)
	return nil
}

// advisoryCacheMux serves the targeted issue GET and the reopen PATCH, and
// counts every search or list request so a test can assert none happened.
func advisoryCacheMux(t *testing.T, num int, state string, lists *atomic.Int64, gets *atomic.Int64, reopened *atomic.Int64) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, _ *http.Request) {
		lists.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			lists.Add(1)
		}
		_ = json.NewEncoder(w).Encode([]any{})
	})
	mux.HandleFunc("/repos/o/r/issues/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/labels") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": advisoryLabelName}})
			return
		}
		switch r.Method {
		case http.MethodPatch:
			reopened.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": num, "title": advisoryTitle, "state": "open"})
		default:
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": num, "title": advisoryTitle, "state": state})
		}
	})
	return mux
}

func openAdvisoryItem(n int) forgestate.Item {
	return forgestate.Item{Kind: forgestate.KindIssue, Number: n, State: "open",
		Labels: []string{advisoryLabelName}, FetchedAt: time.Now(), Source: forgestate.SourceReconcile}
}

// A cached open advisory issue is found without any search or list call:
// first discovery costs one targeted GET to confirm the title, and every
// later lookup is answered by the cache alone.
func TestFindAdvisoryIssue_CacheHitAvoidsList(t *testing.T) {
	var lists, gets, reopened atomic.Int64
	srv := httptest.NewServer(advisoryCacheMux(t, 42, "open", &lists, &gets, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"r"})
	store := &fakeForgeState{
		snap:   forgestate.Snapshot{Repo: "o/r", Ready: true, Issues: map[int]forgestate.Item{42: openAdvisoryItem(42), 7: {Number: 7, State: "open"}}},
		snapOK: true, readItem: openAdvisoryItem(42), readOK: true,
	}
	useAdvisoryForgeState(t, store)

	num, err := c.findAdvisoryIssue(context.Background(), "o", "r")
	if err != nil || num != 42 {
		t.Fatalf("first lookup = %d, %v; want 42", num, err)
	}
	if gets.Load() != 1 || lists.Load() != 0 {
		t.Fatalf("first discovery must cost exactly one targeted GET and no list: gets=%d lists=%d", gets.Load(), lists.Load())
	}
	num, err = c.findAdvisoryIssue(context.Background(), "o", "r")
	if err != nil || num != 42 {
		t.Fatalf("second lookup = %d, %v; want 42", num, err)
	}
	if gets.Load() != 1 || lists.Load() != 0 || store.readCalls != 1 {
		t.Fatalf("memoized lookup must be a cache read only: gets=%d lists=%d reads=%d", gets.Load(), lists.Load(), store.readCalls)
	}
	if reopened.Load() != 0 {
		t.Fatal("an open advisory issue must not be reopened")
	}
}

// When the cache's confirming read says the memoized advisory issue is gone
// from the open set, one targeted GET distinguishes closed from deleted; a
// closed one is reopened in place (#4167) and written through to the cache.
func TestFindAdvisoryIssue_CachedClosedIsReopenedWithoutList(t *testing.T) {
	var lists, gets, reopened atomic.Int64
	srv := httptest.NewServer(advisoryCacheMux(t, 42, "closed", &lists, &gets, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"r"})
	store := &fakeForgeState{snapOK: true, snap: forgestate.Snapshot{Repo: "o/r", Ready: true}, readOK: false}
	useAdvisoryForgeState(t, store)
	c.rememberAdvisoryIssue("o/r", 42)

	num, err := c.findAdvisoryIssue(context.Background(), "o", "r")
	if err != nil || num != 42 {
		t.Fatalf("lookup = %d, %v; want reopened 42", num, err)
	}
	if lists.Load() != 0 {
		t.Fatalf("closed path must not list: lists=%d", lists.Load())
	}
	if gets.Load() != 1 || reopened.Load() != 1 {
		t.Fatalf("want one targeted GET and one reopen: gets=%d reopened=%d", gets.Load(), reopened.Load())
	}
	if len(store.mutations) != 1 || store.mutations[0].Op != forgestate.OpReopen || store.mutations[0].Number != 42 {
		t.Fatalf("reopen must be written through to the cache, got %+v", store.mutations)
	}
}

// A memoized number is still confirmed when the cache cannot vouch for it
// (not ready / stale): one targeted GET, never a list.
func TestFindAdvisoryIssue_CacheErrorFallsBackToTargetedGet(t *testing.T) {
	var lists, gets, reopened atomic.Int64
	srv := httptest.NewServer(advisoryCacheMux(t, 42, "open", &lists, &gets, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"r"})
	store := &fakeForgeState{snapOK: true, readErr: forgestate.ErrStale}
	useAdvisoryForgeState(t, store)
	c.rememberAdvisoryIssue("o/r", 42)

	num, err := c.findAdvisoryIssue(context.Background(), "o", "r")
	if err != nil || num != 42 {
		t.Fatalf("lookup = %d, %v; want 42", num, err)
	}
	if gets.Load() != 1 || lists.Load() != 0 {
		t.Fatalf("stale cache must cost one targeted GET and no list: gets=%d lists=%d", gets.Load(), lists.Load())
	}
}

// Before the repo's first reconcile the cache cannot answer, so the legacy
// search/list path runs — and nothing is memoized from an empty result.
func TestFindAdvisoryIssue_CacheNotReadyUsesListPath(t *testing.T) {
	var lists, gets, reopened atomic.Int64
	srv := httptest.NewServer(advisoryCacheMux(t, 42, "open", &lists, &gets, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"r"})
	useAdvisoryForgeState(t, &fakeForgeState{snapOK: true, snap: forgestate.Snapshot{Repo: "o/r", Ready: false}})

	num, err := c.findAdvisoryIssue(context.Background(), "o", "r")
	if err != nil || num != 0 {
		t.Fatalf("lookup = %d, %v; want 0 (nothing found)", num, err)
	}
	if lists.Load() == 0 {
		t.Fatal("an unready cache must fall back to the list path")
	}
	if _, ok := c.advisoryIssueMemo("o/r"); ok {
		t.Fatal("nothing must be memoized when no advisory issue was found")
	}
}

// Without a store installed the lookup is exactly the pre-#13378 list path.
func TestFindAdvisoryIssue_NoStoreUsesListPath(t *testing.T) {
	var lists, gets, reopened atomic.Int64
	srv := httptest.NewServer(advisoryCacheMux(t, 42, "open", &lists, &gets, &reopened))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "o", []string{"r"})
	if _, err := c.findAdvisoryIssue(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if lists.Load() == 0 || gets.Load() != 0 {
		t.Fatalf("no store: want list path only, lists=%d gets=%d", lists.Load(), gets.Load())
	}
}

// *forgestate.Store satisfies the reader slice the advisory lookup uses.
var _ forgeStateReader = (*forgestate.Store)(nil)

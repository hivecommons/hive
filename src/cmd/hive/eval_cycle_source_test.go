package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The webhook patcher upserts and removes single items in the stored
// snapshot copy-on-write, with no GitHub call of its own.
func TestSnapshotPatcherUpsertsAndRemovesWithoutListing(t *testing.T) {
	var target atomic.Pointer[github.ActionableResult]
	var patched int
	p := &snapshotPatcher{target: &target, onPatch: func(n int) { patched += n }}

	// Nothing stored yet: the first cycle enumerates anyway.
	if n := p.apply([]github.WebhookItemSnapshot{{Repo: "org/repo", Number: 1, State: "closed"}}); n != 0 {
		t.Fatalf("patch without a snapshot applied %d", n)
	}
	seed := &github.ActionableResult{
		GeneratedAt: time.Now().Add(-5 * time.Minute),
		Issues:      github.IssueResultFromItems([]github.Issue{{Repo: "org/repo", Number: 1}, {Repo: "org/repo", Number: 2}}),
		TotalByRepo: map[string]github.RepoCounts{"org/repo": {Issues: 2}},
	}
	target.Store(seed)
	n := p.apply([]github.WebhookItemSnapshot{
		{Repo: "org/repo", Number: 1, State: "closed"},
		{Repo: "org/repo", Number: 3, State: "open", Patch: github.WebhookItemPatch{Bucket: github.WebhookBucketActionable, Issue: &github.Issue{Repo: "org/repo", Number: 3}}},
		{Repo: "org/repo", Number: 4, State: "open"}, // unknown bucket, unlisted: left for reconcile
	})
	if n != 2 || patched != 2 {
		t.Fatalf("applied %d (counter %d), want 2", n, patched)
	}
	got := target.Load()
	if got == seed || seed.Issues.Count != 2 {
		t.Fatal("patch must publish a copy, not mutate the stored snapshot")
	}
	if got.Issues.Count != 2 || got.Issues.Items[0].Number == 1 || got.TotalByRepo["org/repo"].Issues != 2 {
		t.Fatalf("patched snapshot = %+v", got.Issues)
	}
}

// evalFixtureServer counts list calls and answers the single-item GETs a
// webhook re-read makes, so a cycle's API footprint can be asserted.
type evalFixtureServer struct {
	srv   *httptest.Server
	fail  atomic.Bool
	mu    sync.Mutex
	paths []string
}

func newEvalFixtureServer(t *testing.T, issues []map[string]any) *evalFixtureServer {
	t.Helper()
	f := &evalFixtureServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if f.fail.Load() && (r.URL.Path == "/repos/org/repo/issues" || r.URL.Path == "/repos/org/repo/pulls") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"unavailable"}`))
			return
		}
		switch {
		case r.URL.Path == "/repos/org/repo/issues":
			_ = json.NewEncoder(w).Encode(issues)
		case r.URL.Path == "/repos/org/repo/pulls":
			_, _ = w.Write([]byte(`[]`))
		case strings.HasPrefix(r.URL.Path, "/repos/org/repo/issues/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "title": "two", "state": "closed", "user": map[string]any{"login": "a"}})
		case r.URL.Path == "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4999,"reset":9999999999}}}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *evalFixtureServer) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == "/repos/org/repo/issues" || p == "/repos/org/repo/pulls" {
			n++
		}
	}
	return n
}

// A cached cycle plans from the stored snapshot without listing; the
// reconcile pass lists and overwrites the patched snapshot; a failed
// reconcile keeps the previous snapshot. Driven through the same
// enumerate / patch / cycle entry points the loop uses.
func TestEvalCycleSourceCachedSkipsListingAndReconcileOverwrites(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	issues := []map[string]any{
		{"number": 1, "title": "one", "state": "open", "user": map[string]any{"login": "a"}, "created_at": "2026-10-20T10:00:00Z"},
		{"number": 2, "title": "two", "state": "open", "user": map[string]any{"login": "b"}, "created_at": "2026-10-20T10:00:00Z"},
	}
	f := newEvalFixtureServer(t, issues)
	gh := github.NewClientForTest(f.srv.URL, "org", []string{"repo"}, logger)
	ctx := context.Background()

	// Reconcile pass (what the cadence tick does).
	var last atomic.Pointer[github.ActionableResult]
	full, err := gh.EnumerateActionable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last.Store(full)
	baseline := f.listCalls()
	if full.Issues.Count != 2 || baseline == 0 {
		t.Fatalf("baseline: issues=%d lists=%d", full.Issues.Count, baseline)
	}

	// Webhook: issue 2 closed. One targeted GET, then the patch; no list.
	snap, err := gh.FetchWebhookItemSnapshot(ctx, "repo", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	p := &snapshotPatcher{target: &last}
	if p.apply([]github.WebhookItemSnapshot{snap}) != 1 {
		t.Fatal("patch not applied")
	}
	if got := last.Load(); got.Issues.Count != 1 || got.Issues.Items[0].Number != 1 {
		t.Fatalf("patched = %+v", got.Issues)
	}
	if f.listCalls() != baseline {
		t.Fatalf("webhook patch listed: %d calls beyond baseline", f.listCalls()-baseline)
	}

	// Cached cycle: the decision pass reads the patched snapshot and does
	// not list. (The cycle's source selection is the part under test; the
	// helper mirrors runEvalCycle's gate.)
	cached := cachedSnapshotFor(evalSourceCached, &last)
	if cached == nil || cached.Issues.Count != 1 {
		t.Fatalf("cached snapshot = %+v", cached)
	}
	if cachedSnapshotFor(evalSourceEnumerate, &last) != nil {
		t.Fatal("enumerate source must not use the cache")
	}
	var empty atomic.Pointer[github.ActionableResult]
	if cachedSnapshotFor(evalSourceCached, &empty) != nil {
		t.Fatal("no snapshot yet must fall back to enumerate")
	}
	if f.listCalls() != baseline {
		t.Fatalf("cached cycle listed: %d calls beyond baseline", f.listCalls()-baseline)
	}

	// Reconcile overwrites the patch with GitHub's truth (both issues open
	// again on the list endpoint).
	full2, err := gh.EnumerateActionable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last.Store(full2)
	if last.Load().Issues.Count != 2 {
		t.Fatal("reconcile must overwrite the patched snapshot")
	}

	// Failed reconcile: GitHub refuses the list transiently. The repo is
	// served from its last-good enumeration and marked stale, so the
	// published snapshot never drops to zero.
	// Failed reconcile: every repo refused, so the pass errors and the
	// governor keeps the snapshot it holds, marked stale for every repo.
	f.fail.Store(true)
	fresh, err := gh.EnumerateActionable(ctx)
	got, ok := actionableAfterGitHubEnumerate(&config.Config{}, fresh, err, logger)
	if ok || got != nil {
		t.Fatalf("failed enumeration must not publish: %+v ok=%v err=%v", got, ok, err)
	}
	kept := keepLastGoodAfterFailedEnumerate(&last, logger)
	if kept == nil || kept != last.Load() {
		t.Fatal("expected the previous snapshot to be kept and published")
	}
	if kept.Issues.Count != 2 || !kept.Stale() || len(kept.StaleRepos) != 1 {
		t.Fatalf("failed reconcile kept %+v stale=%v", kept.Issues, kept.StaleRepos)
	}
	if kept.StaleSince.IsZero() || !kept.StaleSince.Equal(kept.GeneratedAt) {
		t.Fatalf("stale-since marker = %v, want the kept snapshot's generation time %v", kept.StaleSince, kept.GeneratedAt)
	}
	// Idempotent: a second failure does not republish.
	if again := keepLastGoodAfterFailedEnumerate(&last, logger); again != kept || last.Load() != kept {
		t.Fatal("already-stale snapshot must not be republished")
	}
	// A later successful pass clears the stale mark.
	f.fail.Store(false)
	full3, err := gh.EnumerateActionable(ctx)
	if err != nil || full3.Stale() {
		t.Fatalf("recovery: err=%v stale=%v", err, full3 != nil && full3.Stale())
	}
}

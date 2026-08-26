package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The three Start*Watcher goroutines drive the hive's highest-privilege write
// paths — App-authored merges, PR opens and reviews — and had 0% coverage even
// though pkg/github sits at 90% overall and every helper they call is tested.
// What was untested is the wiring: ticker cadence, context cancellation, and
// whether a single bad request file stops the loop for everything behind it.
//
// That gap is quiet in the worst way. A watcher that silently returns on its
// first scan error, or ignores cancellation, breaks every agent write while the
// whole existing suite stays green.

// The shared mock helpers in this package count with a plain *int, which is
// fine for tests that read the counter only after the server is closed. These
// loops are polled while the handler is still serving, so the counter is read
// and written concurrently and has to be atomic — otherwise the test itself is
// the data race, not the code under test.
func countingServer(t *testing.T, hits *atomic.Int64, match func(*http.Request) bool, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if match(r) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
}

func mergeServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	return countingServer(t, hits, func(r *http.Request) bool {
		return r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge")
	}, `{"sha":"deadbeef","merged":true,"message":"merged"}`)
}

func prServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	return countingServer(t, hits, func(r *http.Request) bool {
		return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls")
	}, `{"number":42,"html_url":"https://github.example/o/r/pull/42"}`)
}

func reviewServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	return countingServer(t, hits, func(r *http.Request) bool {
		return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews")
	}, `{"id":1,"state":"APPROVED"}`)
}

// drainAfter stops the watcher and waits for its goroutine to be gone before
// the test's other cleanups run.
//
// t.Cleanup is LIFO, so this must be registered *after* the dir and interval
// helpers: their restores would otherwise write the package globals while a
// live watcher goroutine is still reading them on each tick, which the race
// detector correctly flags — the test would be the data race, not the code.
func drainAfter(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	t.Cleanup(func() {
		cancel()
		// Comfortably more than the 15ms test tick even under -race, which
		// slows every scan down, so the loop has observed cancellation and
		// returned before anything it reads is restored.
		time.Sleep(750 * time.Millisecond)
	})
}

// withReviewDirForLoop mirrors withMergeDir; see its comment.
func withReviewDirForLoop(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	reviewRequestDirForTest = dir
	return dir
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// fastTick shortens a watcher's poll interval for the duration of the test.
func fastTick(t *testing.T, set func(time.Duration), restore time.Duration) {
	t.Helper()
	set(15 * time.Millisecond)
	t.Cleanup(func() { set(restore) })
}

// withMergeDir points the watcher at a temp dir and deliberately does NOT
// restore the previous value.
//
// Start*Watcher gives the caller no way to know its goroutine has exited —
// cancelling the context signals it, but nothing reports when the loop is
// actually gone. The dir hook is read on *every* tick (inside
// processMergeRequests), so a cleanup that restored it would be writing a
// global that a possibly-still-live goroutine is reading, and the race
// detector flags it. That race is in the test, not the code.
//
// Leaving the value set is safe here: every test in this package assigns the
// hook before using it (see the existing watcher tests, which each set it and
// reset to ""), so nothing inherits it. The poll interval, by contrast, is read
// once when the ticker is created, so restoring that is fine and fastTick does.
func withMergeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mergeRequestDirForTest = dir
	return dir
}

// withPRDirForLoop mirrors withMergeDir; see its comment for why the previous
// value is not restored.
func withPRDirForLoop(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prRequestDirForTest = dir
	return dir
}

// --- merge ------------------------------------------------------------------

func TestStartMergeRequestWatcher_DispatchesQueuedRequest(t *testing.T) {
	var merges atomic.Int64
	srv := mergeServer(t, &merges)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	dir := withMergeDir(t)
	fastTick(t, func(d time.Duration) { mergeRequestPollInterval = d }, mergeRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartMergeRequestWatcher(ctx, func(string, int, string, int, string) error { return nil }, nil)

	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 1, Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return merges.Load() > 0 }) {
		t.Fatal("the ticker loop never picked up a queued merge request")
	}
}

func TestStartMergeRequestWatcher_StopsOnContextCancel(t *testing.T) {
	var merges atomic.Int64
	srv := mergeServer(t, &merges)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	dir := withMergeDir(t)
	fastTick(t, func(d time.Duration) { mergeRequestPollInterval = d }, mergeRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartMergeRequestWatcher(ctx, func(string, int, string, int, string) error { return nil }, nil)
	cancel()
	// Give the loop time to observe cancellation before queueing work.
	time.Sleep(80 * time.Millisecond)

	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 2, Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if merges.Load() != 0 {
		t.Fatalf("watcher kept processing after cancellation (merges=%d)", merges.Load())
	}
}

// A malformed file must not wedge the queue behind it. Before, an unreadable or
// unparseable request could stop the scan and every later request would sit
// unprocessed with nothing reporting why.
func TestStartMergeRequestWatcher_SurvivesMalformedRequest(t *testing.T) {
	var merges atomic.Int64
	srv := mergeServer(t, &merges)
	t.Cleanup(srv.Close)
	c := testMergeClient(t, srv.URL)
	dir := withMergeDir(t)
	fastTick(t, func(d time.Duration) { mergeRequestPollInterval = d }, mergeRequestPollInterval)

	if err := os.WriteFile(filepath.Join(dir, "aaa-broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartMergeRequestWatcher(ctx, func(string, int, string, int, string) error { return nil }, nil)

	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 3, Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return merges.Load() > 0 }) {
		t.Fatal("a malformed request file stopped the loop from reaching a valid one")
	}
}

// --- pr ---------------------------------------------------------------------

func TestStartPRRequestWatcher_DispatchesQueuedRequest(t *testing.T) {
	var created atomic.Int64
	srv := prServer(t, &created)
	t.Cleanup(srv.Close)
	c := testClient(t, srv.URL)
	dir := withPRDirForLoop(t)
	fastTick(t, func(d time.Duration) { prRequestPollInterval = d }, prRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartPRRequestWatcher(ctx, func(string, int) error { return nil }, nil, nil)

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "f", Base: "main", Title: "t", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return created.Load() > 0 }) {
		t.Fatal("the ticker loop never picked up a queued PR request")
	}
}

func TestStartPRRequestWatcher_StopsOnContextCancel(t *testing.T) {
	var created atomic.Int64
	srv := prServer(t, &created)
	t.Cleanup(srv.Close)
	c := testClient(t, srv.URL)
	dir := withPRDirForLoop(t)
	fastTick(t, func(d time.Duration) { prRequestPollInterval = d }, prRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartPRRequestWatcher(ctx, func(string, int) error { return nil }, nil, nil)
	cancel()
	time.Sleep(80 * time.Millisecond)

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "g", Base: "main", Title: "t", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if created.Load() != 0 {
		t.Fatalf("watcher kept processing after cancellation (created=%d)", created.Load())
	}
}

// --- review -----------------------------------------------------------------

func TestStartReviewRequestWatcher_DispatchesQueuedRequest(t *testing.T) {
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDirForLoop(t)
	fastTick(t, func(d time.Duration) { reviewRequestPollInterval = d }, reviewRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartReviewRequestWatcher(ctx, func(string, int) error { return nil }, nil)

	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 1, Event: "approve", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return reviewed.Load() > 0 }) {
		t.Fatal("the ticker loop never picked up a queued review request")
	}
}

func TestStartReviewRequestWatcher_StopsOnContextCancel(t *testing.T) {
	var reviewed atomic.Int64
	srv := reviewServer(t, &reviewed)
	t.Cleanup(srv.Close)
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDirForLoop(t)
	fastTick(t, func(d time.Duration) { reviewRequestPollInterval = d }, reviewRequestPollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	drainAfter(t, cancel)
	c.StartReviewRequestWatcher(ctx, func(string, int) error { return nil }, nil)
	cancel()
	time.Sleep(80 * time.Millisecond)

	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 2, Event: "approve", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if reviewed.Load() != 0 {
		t.Fatalf("watcher kept processing after cancellation (reviewed=%d)", reviewed.Load())
	}
}

// A watcher whose request dir cannot be created must disable itself quietly
// rather than panic or leave a goroutine spinning on a directory that is not
// there.
func TestStartWatchers_UncreatableDirDisablesQuietly(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	oldMerge, oldPR, oldReview := mergeRequestDirForTest, prRequestDirForTest, reviewRequestDirForTest
	mergeRequestDirForTest = filepath.Join(blocker, "merge")
	prRequestDirForTest = filepath.Join(blocker, "pr")
	reviewRequestDirForTest = filepath.Join(blocker, "review")
	t.Cleanup(func() {
		mergeRequestDirForTest, prRequestDirForTest, reviewRequestDirForTest = oldMerge, oldPR, oldReview
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.StartMergeRequestWatcher(ctx, nil, nil)
	c.StartPRRequestWatcher(ctx, nil, nil, nil)
	c.StartReviewRequestWatcher(ctx, nil, nil)
}

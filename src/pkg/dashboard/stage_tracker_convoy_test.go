package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// heldStageTrackerBuild is a tracker build the test holds open: each call
// blocks on release and counts itself, standing in for the 40s evidence walk
// on NFS that hivecommons/hive#14681 measured.
type heldStageTrackerBuild struct {
	release chan struct{}
	started chan struct{} // one send per build entered
	builds  atomic.Int64
}

func newHeldStageTrackerBuild() *heldStageTrackerBuild {
	return &heldStageTrackerBuild{release: make(chan struct{}), started: make(chan struct{}, 64)}
}

func (h *heldStageTrackerBuild) build(status *StatusPayload, _ *stagelabels.Store, now time.Time) StageTrackerResponse {
	h.builds.Add(1)
	h.started <- struct{}{}
	<-h.release
	return StageTrackerResponse{GeneratedAt: now, Items: []StageTrackerItem{{Repo: "acme/console", Number: int(h.builds.Load())}}}
}

func (h *heldStageTrackerBuild) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no tracker build started")
	}
}

func convoyServer(t *testing.T, h *heldStageTrackerBuild) *Server {
	t.Helper()
	s := newTestServer()
	deps := testDeps(t)
	deps.Config.Dashboard.StageTracker.CacheTTLS = 3600
	s.deps = deps
	s.stageTrackerBuildFn = h.build
	s.UpdateStatus(minimalPayload())
	return s
}

func getStageTracker(ctx context.Context, s *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	s.handleStageTracker(rr, req)
	return rr
}

// TestStageTrackerInvalidationNeverWaitsForABuild pins the fix for #14681:
// the webhook and status-publish paths invalidate the tracker cache, and
// they must return while a build is in flight. Before, the invalidator took
// the mutex the builder held for the whole build, so every webhook handler
// and the governor's publish queued behind a 40s NFS walk.
func TestStageTrackerInvalidationNeverWaitsForABuild(t *testing.T) {
	h := newHeldStageTrackerBuild()
	s := convoyServer(t, h)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- getStageTracker(context.Background(), s) }()
	h.waitStarted(t)

	invalidated := make(chan bool, 1)
	go func() { invalidated <- s.invalidateStageTrackerCache() }()
	select {
	case served := <-invalidated:
		if !served {
			t.Fatal("invalidate must report the tracker as served once a build began")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("invalidateStageTrackerCache blocked behind the in-flight build")
	}
	// The webhook path goes through the same door.
	done := make(chan struct{})
	go func() { s.invalidateStageTrackerForWebhook(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("invalidateStageTrackerForWebhook blocked behind the in-flight build")
	}

	close(h.release)
	rr := <-first
	if rr.Code != http.StatusOK || rr.Header().Get("X-Hive-Stage-Tracker-Source") != "fresh" {
		t.Fatalf("building request: %d src=%q", rr.Code, rr.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	if n := h.builds.Load(); n != 1 {
		t.Fatalf("builds = %d, want 1", n)
	}
}

// TestStageTrackerBuildsAreSingleFlightAndServeStale: N concurrent polls
// during a rebuild cost one build and return at once with the previous body
// marked stale, so the number of goroutines parked in the handler cannot
// grow with the poll rate — the pile-up the production hive leaked under.
func TestStageTrackerBuildsAreSingleFlightAndServeStale(t *testing.T) {
	h := newHeldStageTrackerBuild()
	s := convoyServer(t, h)

	// Seed the cache with one complete build.
	seed := make(chan *httptest.ResponseRecorder, 1)
	go func() { seed <- getStageTracker(context.Background(), s) }()
	h.waitStarted(t)
	h.release <- struct{}{}
	if rr := <-seed; rr.Code != http.StatusOK {
		t.Fatalf("seed: %d", rr.Code)
	}
	etag := func(rr *httptest.ResponseRecorder) string { return rr.Header().Get("ETag") }
	seedETag := ""
	if rr := getStageTracker(context.Background(), s); rr.Header().Get("X-Hive-Stage-Tracker-Source") != "cache" {
		t.Fatalf("second poll must hit the cache, got %q", rr.Header().Get("X-Hive-Stage-Tracker-Source"))
	} else {
		seedETag = etag(rr)
	}

	// Invalidate (a webhook landed); the next poll starts the one rebuild.
	s.invalidateStageTrackerCache()
	builder := make(chan *httptest.ResponseRecorder, 1)
	go func() { builder <- getStageTracker(context.Background(), s) }()
	h.waitStarted(t)

	baseline := runtime.NumGoroutine()
	const polls = 200
	var wg sync.WaitGroup
	codes := make([]int, polls)
	sources := make([]string, polls)
	for i := 0; i < polls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each poll is also preceded by a webhook invalidation, as in
			// production where ~3 deliveries/s re-stale the cache.
			s.invalidateStageTrackerForWebhook()
			rr := getStageTracker(context.Background(), s)
			codes[i], sources[i] = rr.Code, rr.Header().Get("X-Hive-Stage-Tracker-Source")
		}(i)
	}
	// All polls return while the build is still held open.
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("%d polls did not return while a build was in flight", polls)
	}
	for i := range codes {
		if codes[i] != http.StatusOK || sources[i] != "stale" {
			t.Fatalf("poll %d: %d src=%q, want 200 stale", i, codes[i], sources[i])
		}
	}
	if n := h.builds.Load(); n != 2 {
		t.Fatalf("builds = %d, want 2 (seed + one rebuild for %d polls)", n, polls)
	}
	testutil.Eventually(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= baseline+2 },
		"goroutines after %d stale polls: %d, baseline %d", polls, runtime.NumGoroutine(), baseline)

	// A waiter whose client went away stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Drop the body so this request has nothing stale to serve and must wait.
	s.stageTrackerCacheMu.Lock()
	saved := s.stageTrackerCache
	s.stageTrackerCache = stageTrackerCache{}
	s.stageTrackerCacheMu.Unlock()
	if rr := getStageTracker(ctx, s); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled waiter: %d, want 503", rr.Code)
	}
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache = saved
	s.stageTrackerCacheMu.Unlock()

	// Release the build: the builder gets fresh; because invalidations
	// landed during the build the result is already stale, so the next poll
	// serves it as stale and starts build #3.
	h.release <- struct{}{}
	rr := <-builder
	if rr.Code != http.StatusOK || rr.Header().Get("X-Hive-Stage-Tracker-Source") != "fresh" {
		t.Fatalf("builder: %d src=%q", rr.Code, rr.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	if etag(rr) == seedETag {
		t.Fatal("rebuilt body must carry the new ETag")
	}
	next := make(chan *httptest.ResponseRecorder, 1)
	go func() { next <- getStageTracker(context.Background(), s) }()
	h.waitStarted(t)
	if during := getStageTracker(context.Background(), s); during.Header().Get("X-Hive-Stage-Tracker-Source") != "stale" || etag(during) != etag(rr) {
		t.Fatalf("poll during build #3: src=%q etag=%q want stale %q", during.Header().Get("X-Hive-Stage-Tracker-Source"), etag(during), etag(rr))
	}
	close(h.release)
	if rr := <-next; rr.Header().Get("X-Hive-Stage-Tracker-Source") != "fresh" {
		t.Fatalf("build #3: src=%q", rr.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	if n := h.builds.Load(); n != 3 {
		t.Fatalf("builds = %d, want 3", n)
	}
	// Quiet now: polls hit the cache and start nothing.
	if rr := getStageTracker(context.Background(), s); rr.Header().Get("X-Hive-Stage-Tracker-Source") != "cache" {
		t.Fatalf("quiet poll: src=%q", rr.Header().Get("X-Hive-Stage-Tracker-Source"))
	}
	if n := h.builds.Load(); n != 3 {
		t.Fatalf("quiet poll started build %d", n)
	}
}

// TestStageTrackerFirstWaitersJoinTheOneBuild: with no body yet, concurrent
// first polls all wait on the single in-flight build rather than each
// starting one.
func TestStageTrackerFirstWaitersJoinTheOneBuild(t *testing.T) {
	h := newHeldStageTrackerBuild()
	s := convoyServer(t, h)
	const waiters = 50
	results := make(chan *httptest.ResponseRecorder, waiters)
	for i := 0; i < waiters; i++ {
		go func() { results <- getStageTracker(context.Background(), s) }()
	}
	h.waitStarted(t)
	close(h.release)
	fresh, cached := 0, 0
	for i := 0; i < waiters; i++ {
		select {
		case rr := <-results:
			if rr.Code != http.StatusOK {
				t.Fatalf("waiter: %d", rr.Code)
			}
			switch rr.Header().Get("X-Hive-Stage-Tracker-Source") {
			case "fresh":
				fresh++
			case "cache":
				cached++
			default:
				t.Fatalf("waiter src=%q", rr.Header().Get("X-Hive-Stage-Tracker-Source"))
			}
		case <-time.After(10 * time.Second):
			t.Fatal("waiters did not drain after the build was released")
		}
	}
	if fresh != 1 || cached != waiters-1 || h.builds.Load() != 1 {
		t.Fatalf("fresh=%d cached=%d builds=%d; want 1/%d/1", fresh, cached, h.builds.Load(), waiters-1)
	}
}

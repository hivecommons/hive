package hub

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func backdateTTLFlight[V any](c *ttlFlight[V], key string, age time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	e.at = time.Now().Add(-age)
	c.entries[key] = e
}

func stubStablePromotionRuns(t *testing.T, runs []stablePromotionWorkflowRun, gate <-chan struct{}) *int32 {
	t.Helper()
	resetStablePromotionCaches()
	var calls int32
	orig := stablePromotionFetchRuns
	stablePromotionFetchRuns = func(stablePromotionRunsCutoff, *slog.Logger) ([]stablePromotionWorkflowRun, error) {
		atomic.AddInt32(&calls, 1)
		if gate != nil {
			<-gate
		}
		return runs, nil
	}
	t.Cleanup(func() {
		waitChannelTargetRefreshes(t)
		stablePromotionFetchRuns = orig
		resetStablePromotionCaches()
	})
	return &calls
}

func TestStablePromotionRunsCachedUntilTTL(t *testing.T) {
	calls := stubStablePromotionRuns(t, []stablePromotionWorkflowRun{{RunNumber: 1}}, nil)
	for i := 0; i < 5; i++ {
		if got := stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default()); len(got) != 1 {
			t.Fatalf("call %d returned %d runs, want 1", i, len(got))
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("runs fetched %d times, want 1 while cached", n)
	}

	backdateTTLFlight(&stablePromotionRunsCache, "runs", channelDigestTTL+time.Second)
	stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default())
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("runs fetched %d times after expiry, want 2", n)
	}
}

func TestStablePromotionRunsSingleFlight(t *testing.T) {
	gate := make(chan struct{})
	calls := stubStablePromotionRuns(t, []stablePromotionWorkflowRun{{RunNumber: 1}}, gate)

	const callers = 16
	var wg sync.WaitGroup
	results := make(chan int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- len(stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default()))
		}()
	}
	// Let the leader reach the fetch before releasing it.
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(results)
	for n := range results {
		if n != 1 {
			t.Fatalf("a concurrent caller got %d runs, want 1", n)
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("concurrent callers triggered %d fetches, want 1", n)
	}
}

func TestStablePromotionRunsFailureServesPreviousAndRetries(t *testing.T) {
	var fail atomic.Bool
	var calls int32
	resetStablePromotionCaches()
	orig := stablePromotionFetchRuns
	stablePromotionFetchRuns = func(stablePromotionRunsCutoff, *slog.Logger) ([]stablePromotionWorkflowRun, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, fmt.Errorf("stub failure")
		}
		return []stablePromotionWorkflowRun{{RunNumber: 7}}, nil
	}
	t.Cleanup(func() {
		waitChannelTargetRefreshes(t)
		stablePromotionFetchRuns = orig
		resetStablePromotionCaches()
	})

	stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default())
	fail.Store(true)
	backdateTTLFlight(&stablePromotionRunsCache, "runs", channelDigestTTL+time.Second)
	if got := stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default()); len(got) != 1 || got[0].RunNumber != 7 {
		t.Fatalf("failed refresh returned %+v, want the previous answer", got)
	}
	stablePromotionRuns(stablePromotionRunsCutoff{}, slog.Default())
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("fetches = %d, want 3 — a failed refresh must not be cached", n)
	}
}

func TestGHCRTagGenerationCached(t *testing.T) {
	resetStablePromotionCaches()
	var calls int32
	orig := fetchGHCRTagGeneration
	fetchGHCRTagGeneration = func(_, tag string, _ *slog.Logger) int {
		atomic.AddInt32(&calls, 1)
		return 42
	}
	t.Cleanup(func() {
		fetchGHCRTagGeneration = orig
		resetStablePromotionCaches()
	})

	for i := 0; i < 4; i++ {
		if got := cachedGHCRTagGeneration(ghcrRepoSpoke, ReleaseChannelStable, slog.Default()); got != 42 {
			t.Fatalf("generation = %d, want 42", got)
		}
	}
	cachedGHCRTagGeneration(ghcrRepoSpoke, ReleaseChannelCandidate, slog.Default())
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("registry lookups = %d, want 2 (one per tag)", n)
	}
	backdateTTLFlight(&ghcrTagGenerationCache, ghcrRepoSpoke+":"+ReleaseChannelStable, channelDigestTTL+time.Second)
	cachedGHCRTagGeneration(ghcrRepoSpoke, ReleaseChannelStable, slog.Default())
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("registry lookups = %d after expiry, want 3", n)
	}
}

// Repeated evaluations reuse the GHCR verification, and one evaluation never
// verifies more than stablePromotionMaxVerifiedRuns runs.
func TestStablePromotionEligibleBuildBoundsAndCachesVerification(t *testing.T) {
	now := time.Now().UTC()
	runs := make([]stablePromotionWorkflowRun, 0, 40)
	for i := 0; i < 40; i++ {
		runs = append(runs, stablePromotionWorkflowRun{
			RunNumber: 500 - i,
			HeadSHA:   fmt.Sprintf("%07x", 0xa000000+i),
			UpdatedAt: now.Add(-time.Duration(30+i) * time.Hour).Format(time.RFC3339),
		})
	}
	stubStablePromotionRuns(t, runs, nil)
	var digestCalls int32
	origDigest := ghcrTagDigest
	ghcrTagDigest = func(string, string, *slog.Logger) string {
		atomic.AddInt32(&digestCalls, 1)
		return "" // nothing verifies
	}
	t.Cleanup(func() { waitChannelTargetRefreshes(t); ghcrTagDigest = origDigest })

	if build, at := stablePromotionEligibleBuild(100, "", now, slog.Default()); at != "" || build.Generation != 0 {
		t.Fatalf("unverifiable runs produced %+v at %q", build, at)
	}
	if n := atomic.LoadInt32(&digestCalls); n != stablePromotionMaxVerifiedRuns {
		t.Fatalf("verified %d runs, want the cap %d", n, stablePromotionMaxVerifiedRuns)
	}
	stablePromotionEligibleBuild(100, "", now, slog.Default())
	if n := atomic.LoadInt32(&digestCalls); n != stablePromotionMaxVerifiedRuns {
		t.Fatalf("second evaluation re-verified: %d lookups, want %d", n, stablePromotionMaxVerifiedRuns)
	}
}

func TestStablePromotionFetchRunsCarriesHubToken(t *testing.T) {
	var auth, path string
	var perPage, page string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		perPage = r.URL.Query().Get("per_page")
		page = r.URL.Query().Get("page")
		_, _ = w.Write([]byte(`{"workflow_runs":[{"run_number":9,"head_sha":"abc"}]}`))
	}))
	defer srv.Close()
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { waitChannelTargetRefreshes(t); githubAPIBase = oldBase })
	t.Setenv(hubGitHubTokenEnv, "ghp_test")

	runs, err := stablePromotionFetchRuns(stablePromotionRunsCutoff{}, slog.Default())
	if err != nil || len(runs) != 1 || runs[0].RunNumber != 9 {
		t.Fatalf("runs = %+v, err = %v", runs, err)
	}
	if auth != "Bearer ghp_test" {
		t.Errorf("Authorization = %q, want the hub token", auth)
	}
	if path != "/repos/hivecommons/hive/actions/workflows/docker.yml/runs" {
		t.Errorf("path = %q", path)
	}
	if perPage != "100" || page != "1" {
		t.Errorf("pagination query = per_page %q page %q, want 100/1", perPage, page)
	}
}

func TestStablePromotionFetchRunsPaginatesPastBusyQueues(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		switch page {
		case "1":
			_, _ = w.Write([]byte(`{"workflow_runs":[`))
			for i := 0; i < stablePromotionRunsPerPage; i++ {
				if i > 0 {
					_, _ = w.Write([]byte(`,`))
				}
				_, _ = fmt.Fprintf(w, `{"run_number":%d,"head_sha":"new%04d"}`, 1000-i, i)
			}
			_, _ = w.Write([]byte(`]}`))
		case "2":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"run_number":800,"head_sha":"old"}]}`))
		default:
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
		}
	}))
	defer srv.Close()
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { waitChannelTargetRefreshes(t); githubAPIBase = oldBase })

	runs, err := stablePromotionFetchRuns(stablePromotionRunsCutoff{}, slog.Default())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(runs) != stablePromotionRunsPerPage+1 {
		t.Fatalf("runs = %d, want %d", len(runs), stablePromotionRunsPerPage+1)
	}
	if runs[len(runs)-1].RunNumber != 800 {
		t.Fatalf("last run = %+v, want page-2 run 800", runs[len(runs)-1])
	}
	if fmt.Sprint(pages) != "[1 2]" {
		t.Fatalf("pages = %v, want [1 2]", pages)
	}
}

func writeStablePromotionRunsPage(w http.ResponseWriter, first int, created time.Time) {
	_, _ = w.Write([]byte(`{"workflow_runs":[`))
	for i := 0; i < stablePromotionRunsPerPage; i++ {
		if i > 0 {
			_, _ = w.Write([]byte(`,`))
		}
		at := created.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, `{"run_number":%d,"head_sha":"run%07d","status":"completed","conclusion":"success","created_at":%q,"updated_at":%q}`, first-i, first-i, at, at)
	}
	_, _ = w.Write([]byte(`]}`))
}

func stubStablePromotionRunsServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	resetStablePromotionCaches()
	srv := httptest.NewServer(handler)
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() {
		srv.Close()
		waitChannelTargetRefreshes(t)
		githubAPIBase = oldBase
		resetStablePromotionCaches()
	})
}

// Runs arrive newest-first, so paging stops at the page that reaches the
// current stable build instead of walking every page (#11684).
func TestStablePromotionFetchRunsStopsAtStableCutoff(t *testing.T) {
	newest := time.Date(2033, 5, 18, 12, 0, 0, 0, time.UTC)
	for name, cutoff := range map[string]stablePromotionRunsCutoff{
		"generation":  newStablePromotionRunsCutoff(950, ""),
		"promoted at": newStablePromotionRunsCutoff(0, newest.Add(-30*time.Minute).Format(time.RFC3339)),
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var pages []string
			stubStablePromotionRunsServer(t, func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				mu.Lock()
				pages = append(pages, page)
				mu.Unlock()
				n := 0
				_, _ = fmt.Sscan(page, &n)
				writeStablePromotionRunsPage(w, 1000-(n-1)*stablePromotionRunsPerPage, newest.Add(-time.Duration(n-1)*stablePromotionRunsPerPage*time.Minute))
			})
			runs, err := stablePromotionFetchRuns(cutoff, slog.Default())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(pages) != "[1]" || len(runs) != stablePromotionRunsPerPage {
				t.Fatalf("pages = %v runs = %d, want only page 1", pages, len(runs))
			}
		})
	}
	if c := newStablePromotionRunsCutoff(0, ""); c.reached(stablePromotionWorkflowRun{RunNumber: 1, CreatedAt: newest.Format(time.RFC3339)}) {
		t.Fatal("an unknown stable build must not stop paging")
	}
}

// A fetch that fails part-way is a failure: the truncated list is dropped, no
// build becomes eligible from it, and the status reports the error (#11684).
func TestStablePromotionPartialRunsFetchDoesNotDriveEligibility(t *testing.T) {
	now := time.Now().UTC()
	stubStablePromotionRunsServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		writeStablePromotionRunsPage(w, 1000, now.Add(-48*time.Hour))
	})
	var digestCalls int32
	origDigest := ghcrTagDigest
	ghcrTagDigest = func(_ string, tag string, _ *slog.Logger) string {
		atomic.AddInt32(&digestCalls, 1)
		return "sha256:" + tag
	}
	t.Cleanup(func() { ghcrTagDigest = origDigest })

	if runs, err := stablePromotionFetchRuns(stablePromotionRunsCutoff{}, slog.Default()); err == nil || runs != nil {
		t.Fatalf("partial fetch = %d runs, err %v; want nil runs and an error", len(runs), err)
	}
	if build, at := stablePromotionEligibleBuild(0, "", now, slog.Default()); at != "" || build.Generation != 0 {
		t.Fatalf("truncated runs produced eligible build %+v at %q", build, at)
	}
	if n := atomic.LoadInt32(&digestCalls); n != 0 {
		t.Fatalf("verified %d builds from a truncated run list, want 0", n)
	}
	ok, errText, at := stablePromotionRunsFetchState()
	if ok == nil || *ok || !strings.Contains(errText, "502") || at == nil {
		t.Fatalf("fetch state = ok %v err %q at %v, want a reported failure", ok, errText, at)
	}
}

// The whole fetch shares one deadline: pages that would each fit a per-page
// budget still fail once the overall budget is spent.
func TestStablePromotionFetchRunsOverallDeadline(t *testing.T) {
	origTimeout := stablePromotionRunsFetchTimeout
	stablePromotionRunsFetchTimeout = 250 * time.Millisecond
	t.Cleanup(func() { stablePromotionRunsFetchTimeout = origTimeout })
	var served int32
	stubStablePromotionRunsServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		atomic.AddInt32(&served, 1)
		writeStablePromotionRunsPage(w, 1000, time.Now().UTC())
	})
	start := time.Now()
	runs, err := stablePromotionFetchRuns(stablePromotionRunsCutoff{}, slog.Default())
	if err == nil || runs != nil {
		t.Fatalf("fetch past the overall deadline = %d runs, err %v; want an error", len(runs), err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fetch took %v, want it bounded by the overall deadline", elapsed)
	}
	if n := atomic.LoadInt32(&served); n >= stablePromotionRunPages {
		t.Fatalf("served %d pages, want the overall deadline to stop paging early", n)
	}
}

// A persistent failure warns once, recovery logs once, and a new failure after
// recovery warns again (#11684).
func TestStablePromotionRunsFetchLogsOncePerTransition(t *testing.T) {
	resetStablePromotionCaches()
	t.Cleanup(resetStablePromotionCaches)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	fail := fmt.Errorf("context deadline exceeded")
	for _, err := range []error{fail, fail, fail, nil, nil, fail, fail} {
		recordStablePromotionRunsFetch(err, logger)
	}
	out := buf.String()
	if n := strings.Count(out, "GitHub runs fetch failed"); n != 2 {
		t.Fatalf("WARN logged %d times, want 2 (one per transition):\n%s", n, out)
	}
	if n := strings.Count(out, "GitHub runs fetch recovered"); n != 1 {
		t.Fatalf("recovery logged %d times, want 1:\n%s", n, out)
	}
	resetStablePromotionCaches()
	if ok, _, at := stablePromotionRunsFetchState(); ok != nil || at != nil {
		t.Fatalf("reset fetch state = ok %v at %v, want unknown", ok, at)
	}
	buf.Reset()
	recordStablePromotionRunsFetch(nil, logger)
	if buf.Len() != 0 {
		t.Fatalf("a first successful fetch logged %q, want nothing", buf.String())
	}
}

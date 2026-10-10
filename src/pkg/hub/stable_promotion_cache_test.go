package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun {
		atomic.AddInt32(&calls, 1)
		if gate != nil {
			<-gate
		}
		return runs
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
		if got := stablePromotionRuns(slog.Default()); len(got) != 1 {
			t.Fatalf("call %d returned %d runs, want 1", i, len(got))
		}
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("runs fetched %d times, want 1 while cached", n)
	}

	backdateTTLFlight(&stablePromotionRunsCache, "runs", channelDigestTTL+time.Second)
	stablePromotionRuns(slog.Default())
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
			results <- len(stablePromotionRuns(slog.Default()))
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
	stablePromotionFetchRuns = func(*slog.Logger) []stablePromotionWorkflowRun {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil
		}
		return []stablePromotionWorkflowRun{{RunNumber: 7}}
	}
	t.Cleanup(func() {
		waitChannelTargetRefreshes(t)
		stablePromotionFetchRuns = orig
		resetStablePromotionCaches()
	})

	stablePromotionRuns(slog.Default())
	fail.Store(true)
	backdateTTLFlight(&stablePromotionRunsCache, "runs", channelDigestTTL+time.Second)
	if got := stablePromotionRuns(slog.Default()); len(got) != 1 || got[0].RunNumber != 7 {
		t.Fatalf("failed refresh returned %+v, want the previous answer", got)
	}
	stablePromotionRuns(slog.Default())
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

	if build, at := stablePromotionEligibleBuild(100, now, slog.Default()); at != "" || build.Generation != 0 {
		t.Fatalf("unverifiable runs produced %+v at %q", build, at)
	}
	if n := atomic.LoadInt32(&digestCalls); n != stablePromotionMaxVerifiedRuns {
		t.Fatalf("verified %d runs, want the cap %d", n, stablePromotionMaxVerifiedRuns)
	}
	stablePromotionEligibleBuild(100, now, slog.Default())
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

	runs := stablePromotionFetchRuns(slog.Default())
	if len(runs) != 1 || runs[0].RunNumber != 9 {
		t.Fatalf("runs = %+v", runs)
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

	runs := stablePromotionFetchRuns(slog.Default())
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

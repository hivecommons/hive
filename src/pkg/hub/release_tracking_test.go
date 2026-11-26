package hub

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetImageBranchCache clears the image-branch discovery cache, its failure
// backoff and its clock for one test, and restores them afterwards.
func resetImageBranchCache(t *testing.T) func(time.Duration) {
	t.Helper()
	imageBranchMu.Lock()
	savedCache, savedAt, savedFailed := imageBranchCache, imageBranchCachedAt, imageBranchFailedAt
	imageBranchCache, imageBranchCachedAt, imageBranchFailedAt = nil, time.Time{}, time.Time{}
	imageBranchMu.Unlock()
	var mu sync.Mutex
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	origNow := imageBranchNow
	imageBranchNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	t.Cleanup(func() {
		imageBranchNow = origNow
		imageBranchMu.Lock()
		imageBranchCache, imageBranchCachedAt, imageBranchFailedAt = savedCache, savedAt, savedFailed
		imageBranchMu.Unlock()
	})
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

// TestDiscoveredImageBranchesBacksOffAfterFailedLookup (#19725): a failed GHCR
// tag-list lookup used to leave the cache nil, so every trackedBranchList call
// fetched again with no delay. Within imageBranchFailureBackoff the previous
// answer must be served with no new request; after it, the lookup retries.
func TestDiscoveredImageBranchesBacksOffAfterFailedLookup(t *testing.T) {
	advance := resetImageBranchCache(t)
	var tokenHits int32
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			atomic.AddInt32(&tokenHits, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable")) // not JSON: the lookup fails
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if got := discoveredImageBranches(); len(got) != 0 {
		t.Fatalf("failed lookup returned %v, want nothing", got)
	}
	if n := atomic.LoadInt32(&tokenHits); n != 1 {
		t.Fatalf("token requests after the first lookup = %d, want 1", n)
	}
	for i := 0; i < 5; i++ {
		advance(5 * time.Second)
		discoveredImageBranches()
	}
	if n := atomic.LoadInt32(&tokenHits); n != 1 {
		t.Fatalf("token requests during the backoff = %d, want 1 (no retry before %s)", n, imageBranchFailureBackoff)
	}
	advance(imageBranchFailureBackoff)
	discoveredImageBranches()
	if n := atomic.LoadInt32(&tokenHits); n != 2 {
		t.Fatalf("token requests after the backoff = %d, want 2", n)
	}
}

// TestDiscoveredImageBranchesReleaseTrackingDisabled (#19726): the switch
// short-circuits discovery before any GHCR or GitHub request.
func TestDiscoveredImageBranchesReleaseTrackingDisabled(t *testing.T) {
	resetImageBranchCache(t)
	t.Setenv(hubReleaseTrackingDisableEnv, "1")
	var hits int32
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"token":"anon","tags":["v5-latest"]}`))
	})
	if got := discoveredImageBranches(); got != nil {
		t.Fatalf("discovery with release tracking disabled = %v, want nil", got)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("requests with release tracking disabled = %d, want 0", n)
	}
}

func TestReleaseTrackingDisabledValues(t *testing.T) {
	for v, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false,
		"1": true, "true": true, "TRUE": true, " yes ": true,
	} {
		t.Setenv(hubReleaseTrackingDisableEnv, v)
		if got := releaseTrackingDisabled(); got != want {
			t.Errorf("%s=%q: disabled = %v, want %v", hubReleaseTrackingDisableEnv, v, got, want)
		}
	}
}

// TestStartBackgroundPollers_ReleaseTrackingDisabled (#19726): with
// HIVE_HUB_RELEASE_TRACKING_DISABLED set, starting the pollers makes no
// release-tracking request to GitHub or GHCR: no branch-tip read, no GHCR
// token or tag list, no package page for image pulls. Without the switch the
// SHA poller's pre-loop fetch reads /branches/v2 from the same fixture (see
// TestStartBackgroundPollers_LaunchesAndStopsOnCancel).
func TestStartBackgroundPollers_ReleaseTrackingDisabled(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	// Unset HIVE_HUB_CUSTOMER_STORE is colony (#19972): the boot opens
	// colony.db in the temp dirs, so close it when the test ends.
	t.Cleanup(closeHubCustomerStore)
	resetSHACaches(t)
	resetImageBranchCache(t)
	resetChannelTargetCache()
	t.Cleanup(resetChannelTargetCache)
	t.Setenv(hubReleaseTrackingDisableEnv, "true")

	oldSHAsPath := latestSHAsPath
	latestSHAsPath = t.TempDir() + "/latest-shas.json"
	defer func() { latestSHAsPath = oldSHAsPath }()

	var mu sync.Mutex
	var tracked []string
	srv := fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.Contains(p, "/branches") || strings.Contains(p, "/token") ||
			strings.Contains(p, "/tags/list") || strings.Contains(p, "/manifests/") ||
			strings.Contains(p, "/pkgs/container") {
			mu.Lock()
			tracked = append(tracked, r.Method+" "+r.URL.String())
			mu.Unlock()
		}
		switch {
		case strings.Contains(p, "/token"):
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(p, "/branches/"):
			_, _ = w.Write([]byte(`{"commit":{"sha":"abcdef1234567890","commit":{"message":"m"}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv.Config.SetKeepAlivesEnabled(false)

	oldPullURL := pullPackagePageURL
	pullPackagePageURL = srv.URL + "/pkgs/container/hive"
	defer func() { pullPackagePageURL = oldPullURL }()

	s := &HubServer{
		logger:     slog.Default(),
		saveCh:     make(chan struct{}, 1),
		hubGitHash: "abc1234",
	}
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := s.StartBackgroundPollers(ctx)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pollers did not stop on cancelled context: done channel still open after 10s")
	}
	waitForCommitOrderResolvers(t)

	if sha := getLatestSHAForBranch("v2"); sha != "" {
		t.Errorf("v2 SHA = %q with release tracking disabled, want unknown", sha)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tracked) != 0 {
		t.Fatalf("release-tracking requests with %s set: %v", hubReleaseTrackingDisableEnv, tracked)
	}
}

// TestDiscoveredImageBranchesTagListFailureBacksOff: the token request works
// but tags/list fails. That must count as a failed lookup (previous answer, 60 s
// backoff), not as an empty registry cached for imageBranchCacheTTL.
func TestDiscoveredImageBranchesTagListFailureBacksOff(t *testing.T) {
	advance := resetImageBranchCache(t)
	var tagHits int32
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/token") {
			_, _ = w.Write([]byte(`{"token":"anon"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/tags/list") {
			atomic.AddInt32(&tagHits, 1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	discoveredImageBranches()
	advance(5 * time.Second)
	discoveredImageBranches()
	if n := atomic.LoadInt32(&tagHits); n != 1 {
		t.Fatalf("tags/list requests inside the backoff = %d, want 1", n)
	}
	imageBranchMu.RLock()
	cached := imageBranchCache
	imageBranchMu.RUnlock()
	if cached != nil {
		t.Fatalf("failed tag list was cached as %v, want nothing cached", cached)
	}
	advance(imageBranchFailureBackoff)
	discoveredImageBranches()
	if n := atomic.LoadInt32(&tagHits); n != 2 {
		t.Fatalf("tags/list requests after the backoff = %d, want 2", n)
	}
}

// TestStablePromotionStatusReleaseTrackingDisabled: the stable-promotion status
// makes no GitHub or GHCR request when release tracking is off.
func TestStablePromotionStatusReleaseTrackingDisabled(t *testing.T) {
	t.Setenv(hubReleaseTrackingDisableEnv, "true")
	var hits int32
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	})
	s := newTestHubServer("secret")
	_ = s.stablePromotionStatus(channelTargetPlaceholders())
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("requests with release tracking disabled = %d, want 0", n)
	}
}

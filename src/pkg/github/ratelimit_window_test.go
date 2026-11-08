package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Regression coverage for kubestellar/hive#5733.
//
// /api/status → .ghRateLimits.core.remaining reported the FULL limit while most
// of the App installation's budget was spent, and moved backwards while doing
// it. Measured, one sample per minute, hive's value against a direct
// GET /rate_limit on the same installation:
//
//	20:56  hive 6941   actual 6815
//	20:57  hive 7100   actual 6741   <- UP, while real usage climbed
//	21:02  hive 7100   actual 6286   <- pinned at the limit for six minutes
//
// The cause is that a just-minted installation token transiently reports a
// fresh, EMPTY bucket for a budget that is genuinely shared, and the dashboard
// latched that reading. The clamp makes the reported value monotone within a
// window; these tests pin both the clamp and — importantly — the reason the
// obvious version of it does not work.

// windowStart is a fixed clock base so every case reads in wall-clock terms.
var windowStart = time.Date(2026, 9, 2, 20, 50, 0, 0, time.UTC)

// trackerAt returns a tracker whose clock the test drives.
func trackerAt(now *time.Time) *rateLimitTracker {
	return &rateLimitTracker{now: func() time.Time { return *now }}
}

func entry(limit, remaining int, reset time.Time) RateLimitEntry {
	return RateLimitEntry{Limit: limit, Remaining: remaining, Reset: reset}
}

// TestRateLimitTracker_NeverReportsMoreHeadroomWithinAWindow replays the shape
// of the reported trace: a genuine decline, interrupted by the fresh-bucket
// artifact, which on the live hive was reported as full headroom.
func TestRateLimitTracker_NeverReportsMoreHeadroomWithinAWindow(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)
	// The artifact carries its own, LATER reset — the fresh bucket reports a
	// fresh window. This is measured, not invented: 1788386085 vs 1788385930.
	artifactReset := reset.Add(155 * time.Second)

	type step struct {
		afterMinutes int
		raw          RateLimitEntry
		wantReported int
	}
	// Raw readings mirror column B (real consumption), with the empty-bucket
	// artifact injected where column A jumped back to 7100.
	steps := []step{
		{1, entry(7100, 7078, reset), 7078},
		{2, entry(7100, 7012, reset), 7012},
		{3, entry(7100, 6881, reset), 6881},
		{4, entry(7100, 6815, reset), 6815},
		{5, entry(7100, 7100, artifactReset), 6815}, // <- the artifact
		{6, entry(7100, 7100, artifactReset), 6815}, // <- and again
		{7, entry(7100, 6459, reset), 6459},         // a real reading resumes
		{8, entry(7100, 7100, artifactReset), 6459},
		{9, entry(7100, 6286, reset), 6286},
	}

	prev := 1 << 30
	for _, s := range steps {
		now = windowStart.Add(time.Duration(s.afterMinutes) * time.Minute)
		got := tr.observe("core", s.raw)
		if got.Remaining != s.wantReported {
			t.Errorf("minute %d: reported remaining = %d, want %d (raw was %d)",
				s.afterMinutes, got.Remaining, s.wantReported, s.raw.Remaining)
		}
		if got.Remaining > prev {
			t.Errorf("minute %d: reported remaining ROSE from %d to %d within one window — "+
				"this is the false-headroom state #5733 is about",
				s.afterMinutes, prev, got.Remaining)
		}
		prev = got.Remaining
	}
}

// TestRateLimitTracker_RejectsFreshBucketCarryingALaterReset is the case a
// naive clamp gets wrong, and the reason this code keys on wall-clock expiry
// rather than on the reset value.
//
// "If reset has not advanced, clamp to the minimum seen" is the obvious
// implementation. It fails here: the fresh-bucket artifact DOES advance the
// reset, because a newly minted token reports its own fresh window. Such a
// clamp would treat every re-mint as a rollover and accept precisely the
// readings it exists to reject.
func TestRateLimitTracker_RejectsFreshBucketCarryingALaterReset(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	tr.observe("core", entry(7100, 6815, reset))

	now = windowStart.Add(time.Minute)
	got := tr.observe("core", entry(7100, 7100, reset.Add(155*time.Second)))

	if got.Remaining != 6815 {
		t.Fatalf("reported remaining = %d, want 6815 — a full bucket arriving with a "+
			"LATER reset while the current window is still open is a re-minted token "+
			"describing its own bucket, not a rollover", got.Remaining)
	}
	if !got.Reset.Equal(reset) {
		t.Errorf("reported reset = %v, want the real window's %v; the drifting reset is "+
			"what made the card's reset 8.5 minutes adrift of reality", got.Reset, reset)
	}
}

// TestRateLimitTracker_AcceptsGenuineRollover: once the window's reset has
// actually passed, a restored budget is real and must be reported. A clamp that
// never lets the number rise would pin the card low forever.
func TestRateLimitTracker_AcceptsGenuineRollover(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	tr.observe("core", entry(7100, 6286, reset))

	// One second before the reset: still the old window, still clamped.
	now = reset.Add(-time.Second)
	if got := tr.observe("core", entry(7100, 7100, reset.Add(time.Hour))); got.Remaining != 6286 {
		t.Fatalf("before the reset: remaining = %d, want the clamped 6286", got.Remaining)
	}

	// At the reset: the window really has rolled over.
	now = reset
	newReset := reset.Add(time.Hour)
	got := tr.observe("core", entry(7100, 7100, newReset))
	if got.Remaining != 7100 {
		t.Fatalf("after the reset: remaining = %d, want 7100 — a real rollover must be "+
			"reported or the card pins low forever", got.Remaining)
	}
	if !got.Reset.Equal(newReset) {
		t.Errorf("reset = %v, want the new window's %v", got.Reset, newReset)
	}
}

// TestRateLimitTracker_TracksGenuineDecline: the clamp must not freeze a
// correctly falling value.
func TestRateLimitTracker_TracksGenuineDecline(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	for i, remaining := range []int{7078, 7012, 6881, 6815, 6741} {
		now = windowStart.Add(time.Duration(i) * time.Minute)
		got := tr.observe("core", entry(7100, remaining, reset))
		if got.Remaining != remaining {
			t.Fatalf("step %d: reported %d, want the observed %d — a falling reading is "+
				"real and must pass through", i, got.Remaining, remaining)
		}
	}
}

// TestRateLimitTracker_ObservedAtHoldsWhileClamped pins the staleness signal.
// The card used to show only `reset`, which moves independently of when the
// sample was taken — so a value that had stopped updating looked exactly like a
// fresh one. A held reading keeps the timestamp of the observation it actually
// came from.
func TestRateLimitTracker_ObservedAtHoldsWhileClamped(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	first := tr.observe("core", entry(7100, 6815, reset))
	if !first.ObservedAt.Equal(windowStart) {
		t.Fatalf("observed_at = %v, want the observation time %v", first.ObservedAt, windowStart)
	}

	now = windowStart.Add(6 * time.Minute)
	held := tr.observe("core", entry(7100, 7100, reset.Add(155*time.Second)))
	if !held.ObservedAt.Equal(windowStart) {
		t.Errorf("observed_at = %v, want it held at %v — a held value that advertised a "+
			"fresh timestamp would be exactly as misleading as the wrong number",
			held.ObservedAt, windowStart)
	}

	// A newly accepted reading re-stamps it.
	now = windowStart.Add(7 * time.Minute)
	fresh := tr.observe("core", entry(7100, 6286, reset))
	if !fresh.ObservedAt.Equal(now) {
		t.Errorf("observed_at = %v, want the new observation time %v", fresh.ObservedAt, now)
	}
}

// TestRateLimitTracker_UnreportedBucketIsPassedThrough: /rate_limit omits
// buckets in some configurations, and the zero entry must not become a tracked
// window — otherwise an absent bucket would pin at zero and read as "exhausted".
func TestRateLimitTracker_UnreportedBucketIsPassedThrough(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)

	if got := tr.observe("graphql", RateLimitEntry{}); got != (RateLimitEntry{}) {
		t.Fatalf("empty entry = %+v, want it passed through untouched", got)
	}

	// And a later real reading for that bucket is accepted normally.
	reset := windowStart.Add(time.Hour)
	if got := tr.observe("graphql", entry(5000, 4998, reset)); got.Remaining != 4998 {
		t.Fatalf("remaining = %d, want 4998; the empty entry must not have pinned a window",
			got.Remaining)
	}
}

// TestRateLimitTracker_BucketsAreIndependent: core, search and graphql have
// separate budgets and separate windows.
func TestRateLimitTracker_BucketsAreIndependent(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	tr.observe("core", entry(7100, 100, reset))
	got := tr.observe("search", entry(30, 29, reset))

	if got.Remaining != 29 {
		t.Fatalf("search remaining = %d, want 29 — core's low value must not clamp search",
			got.Remaining)
	}
}

// TestRateLimitTracker_ConcurrentObservationsAreSafe: the tracker hangs off a
// shared *Client, and /api/status and /api/gh-rate-limits can both be in flight.
// Run with -race.
func TestRateLimitTracker_ConcurrentObservationsAreSafe(t *testing.T) {
	now := windowStart
	tr := trackerAt(&now)
	reset := windowStart.Add(time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.observe("core", entry(7100, 7000-i, reset))
		}(i)
	}
	wg.Wait()

	if got := tr.observe("core", entry(7100, 7100, reset)); got.Remaining > 7000 {
		t.Fatalf("remaining = %d, want the lowest observed (<= 7000)", got.Remaining)
	}
}

// TestRateLimits_ClampAppliesThroughTheClient is the end-to-end path: the same
// artifact served over HTTP must not reach a caller of RateLimits(). The clamp
// lives on the client precisely so that both /api/status and
// /api/gh-rate-limits inherit it.
func TestRateLimits_ClampAppliesThroughTheClient(t *testing.T) {
	// Start from an empty core snapshot: this test is about /rate_limit alone.
	resetRESTAccountingForTest(t, time.Now())

	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	artifactReset := reset.Add(155 * time.Second)

	var mu sync.Mutex
	call := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		// First call: a real, partly-spent bucket. Second: the fresh-bucket
		// artifact, full and carrying its own later reset.
		remaining, rs := 6815, reset
		if n > 1 {
			remaining, rs = 7100, artifactReset
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": map[string]any{
				"core": map[string]any{"limit": 7100, "remaining": remaining, "reset": rs.Unix()},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})

	first, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if first.Core.Remaining != 6815 {
		t.Fatalf("first read = %d, want the observed 6815", first.Core.Remaining)
	}
	if first.Core.ObservedAt.IsZero() {
		t.Error("observed_at is zero; the dashboard cannot show staleness without it")
	}

	second, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if second.Core.Remaining != 6815 {
		t.Fatalf("second read = %d, want 6815 — the full-bucket artifact reached the caller, "+
			"which is the false 100%% headroom of #5733", second.Core.Remaining)
	}
}

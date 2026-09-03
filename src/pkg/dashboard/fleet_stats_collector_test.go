package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// newFleetTestClient builds a github.Client pointed at a test server that
// answers /search/issues with a total keyed by qualifier substrings.
func newFleetTestClient(t *testing.T, byQualifier map[string]int) *ghpkg.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		total := 0
		for sub, n := range byQualifier {
			if strings.Contains(q, sub) {
				total = n
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ghpkg.NewClient("fake", "org", []string{"repo"}, slog.Default(), "")
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c.GoGitHub().BaseURL = base
	return c
}

func TestFleetStatsCollector_SnapshotBeforeCollect(t *testing.T) {
	fc := NewFleetStatsCollector(newFleetTestClient(t, nil), "bot", "org", slog.Default())
	_, ready := fc.Snapshot()
	if ready {
		t.Error("ready should be false before first collect")
	}
}

func TestFleetStatsCollector_Collect(t *testing.T) {
	c := newFleetTestClient(t, map[string]int{
		"is:merged merged:>=":             20,
		"is:closed is:unmerged closed:>=": 3,
		`"CVE-"`:                          6,
	})
	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	fc.collect(context.Background())

	snap, ready := fc.Snapshot()
	if !ready {
		t.Fatal("ready should be true after collect")
	}
	want := ghpkg.FleetContribCounts{PRsMerged: 20, PRsRejected: 3, CVEsClosed: 6}
	if snap != want {
		t.Errorf("got %+v, want %+v", snap, want)
	}
}

func TestFleetStatsCollector_NilAndEmpty(t *testing.T) {
	// nil collector
	var nilFc *FleetStatsCollector
	if _, ready := nilFc.Snapshot(); ready {
		t.Error("nil collector Snapshot ready should be false")
	}
	nilFc.Start(context.Background()) // must not panic

	// empty author/org: Start returns immediately, stays not-ready
	fc := NewFleetStatsCollector(newFleetTestClient(t, nil), "", "", slog.Default())
	fc.Start(context.Background())
	if _, ready := fc.Snapshot(); ready {
		t.Error("empty-config collector should never be ready")
	}
}

func TestFleetStatsCollector_StartCancels(t *testing.T) {
	// Disable the startup jitter: it exists to spread a 50-spoke fleet across
	// the GitHub search rate-limit window, and would otherwise delay this
	// test's initial collect by up to minutes.
	origJitter := fleetStatsStartupJitterMax
	fleetStatsStartupJitterMax = 0
	defer func() { fleetStatsStartupJitterMax = origJitter }()

	c := newFleetTestClient(t, map[string]int{"is:merged merged:>=": 1})
	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fc.Start(ctx); close(done) }()

	// Wait until the up-front collect has completed (ready flips true) before
	// cancelling, so the assertion isn't racing the initial GitHub call.
	deadline := time.After(5 * time.Second)
	for {
		if _, ready := fc.Snapshot(); ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("initial collect never completed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Cancel and confirm the loop exits promptly.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestFleetStatsCollector_TickerReCollects(t *testing.T) {
	// Disable the startup jitter: it exists to spread a 50-spoke fleet across
	// the GitHub search rate-limit window, and would otherwise delay this
	// test's initial collect by up to minutes.
	origJitter := fleetStatsStartupJitterMax
	fleetStatsStartupJitterMax = 0
	defer func() { fleetStatsStartupJitterMax = origJitter }()

	// Shrink the interval so the ticker branch fires within the test.
	orig := fleetStatsCollectInterval
	fleetStatsCollectInterval = 20 * time.Millisecond
	defer func() { fleetStatsCollectInterval = orig }()

	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "items": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := ghpkg.NewClient("fake", "org", []string{"repo"}, slog.Default(), "")
	base, _ := url.Parse(srv.URL + "/")
	c.GoGitHub().BaseURL = base

	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fc.Start(ctx); close(done) }()

	// Wait for at least two collect rounds (initial + one ticker fire); each
	// round is 3 search requests, so >3 hits proves the ticker branch ran.
	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(&hits) <= 3 {
		select {
		case <-deadline:
			t.Fatalf("ticker branch never re-collected (hits=%d)", atomic.LoadInt32(&hits))
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestFleetStatsCollector_CollectError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := ghpkg.NewClient("fake", "org", []string{"repo"}, slog.Default(), "")
	base, _ := url.Parse(srv.URL + "/")
	c.GoGitHub().BaseURL = base

	// Collapse the retry backoff so the test exercises the full retry ladder
	// without waiting minutes of real time.
	restore := fleetStatsRetryBaseDelay
	fleetStatsRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { fleetStatsRetryBaseDelay = restore })

	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	fc.collect(context.Background())
	// A failed collect must leave the collector not-ready (never a fake zero).
	if _, ready := fc.Snapshot(); ready {
		t.Error("collector should stay not-ready after a failed collect")
	}
	if !fc.CollectedAt().IsZero() {
		t.Error("CollectedAt must stay zero when no collect has ever succeeded")
	}
}

// TestFleetStatsCollector_RetriesThenSucceeds is the regression guard for the
// fleet-wide stat collapse: the GitHub search API allows only 30 requests per
// minute, so a rolling restart rate-limits most spokes on their first attempt.
// Before retrying, a single rejection dropped that hive out of the public total
// for a full 30-minute cycle. A transient failure must not cost a hive its
// contribution.
func TestFleetStatsCollector_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		// Fail every search of the first collect attempt, then succeed.
		if calls.Add(1) <= 1 {
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 7, "items": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := ghpkg.NewClient("fake", "org", []string{"repo"}, slog.Default(), "")
	base, _ := url.Parse(srv.URL + "/")
	c.GoGitHub().BaseURL = base

	restore := fleetStatsRetryBaseDelay
	fleetStatsRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { fleetStatsRetryBaseDelay = restore })

	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	fc.collect(context.Background())

	counts, ready := fc.Snapshot()
	if !ready {
		t.Fatal("collector must be ready after a retry succeeds")
	}
	if counts.PRsMerged != 7 {
		t.Errorf("PRsMerged = %d, want 7", counts.PRsMerged)
	}
	if fc.CollectedAt().IsZero() {
		t.Error("CollectedAt must be set after a successful collect")
	}
}

// TestFleetStatsCollector_PersistAndReload is the #2329 durability guard: a
// successful collect must be written to the PVC, and a FRESH collector must load
// those exact counts on start — with the ORIGINAL collectedAt, never restamped
// to now — so a spoke restart resumes from its last-known counts (which the
// hub's fleetStatsMaxAge aging can still evaluate) instead of nil.
func TestFleetStatsCollector_PersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-stats.json")

	c := newFleetTestClient(t, map[string]int{
		"is:merged merged:>=":             20,
		"is:closed is:unmerged closed:>=": 3,
		`"CVE-"`:                          6,
	})
	fc := NewFleetStatsCollector(c, "bot", "org", slog.Default())
	fc.EnablePersistence(path) // no file yet: clean no-op, stays not-ready
	if _, ready := fc.Snapshot(); ready {
		t.Fatal("collector should be not-ready before any collect (missing file is a no-op)")
	}
	fc.collect(context.Background())
	want, ready := fc.Snapshot()
	if !ready {
		t.Fatal("collector should be ready after collect")
	}
	origCollectedAt := fc.CollectedAt()
	if origCollectedAt.IsZero() {
		t.Fatal("collectedAt must be set after a successful collect")
	}

	// The file must now exist on the PVC.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected fleet stats file at %s: %v", path, err)
	}

	// A fresh collector (simulating a restart) must resume from the file WITHOUT
	// any collect, reporting the same counts and the same collectedAt.
	fresh := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fresh.EnablePersistence(path)
	got, ready := fresh.Snapshot()
	if !ready {
		t.Fatal("fresh collector should be ready after loading persisted counts")
	}
	if got != want {
		t.Errorf("restored counts = %+v, want %+v", got, want)
	}
	if !fresh.CollectedAt().Equal(origCollectedAt) {
		t.Errorf("restored collectedAt = %v, want original %v (must not be restamped)",
			fresh.CollectedAt(), origCollectedAt)
	}
}

// TestFleetStatsCollector_LoadCorruptOrMissing confirms a missing file and a
// corrupt file both load cleanly as not-ready (zero counts), never a panic and
// never a fake zero contribution.
func TestFleetStatsCollector_LoadCorruptOrMissing(t *testing.T) {
	// Missing file.
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	fc := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fc.EnablePersistence(missing)
	if _, ready := fc.Snapshot(); ready {
		t.Error("missing file should load as not-ready")
	}

	// Corrupt file.
	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	fc2 := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fc2.EnablePersistence(corrupt)
	if snap, ready := fc2.Snapshot(); ready || snap != (ghpkg.FleetContribCounts{}) {
		t.Errorf("corrupt file should load as not-ready/zero, got ready=%v snap=%+v", ready, snap)
	}

	// A well-formed file with a zero collectedAt is treated as absent — the hub
	// would otherwise age it out against a zero timestamp.
	zeroTime := filepath.Join(t.TempDir(), "zero-time.json")
	blob, _ := json.Marshal(map[string]any{"counts": map[string]int{"prs_merged": 5}})
	if err := os.WriteFile(zeroTime, blob, 0o600); err != nil {
		t.Fatalf("write zero-time file: %v", err)
	}
	fc3 := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	fc3.EnablePersistence(zeroTime)
	if _, ready := fc3.Snapshot(); ready {
		t.Error("file with zero collectedAt should load as not-ready")
	}
}

// TestFleetStatsCollector_StartupDelayWithinBound pins the startup jitter to
// its configured window; it is what spreads the fleet's first collect across
// the search rate-limit window instead of stampeding it.
func TestFleetStatsCollector_StartupDelayWithinBound(t *testing.T) {
	fc := NewFleetStatsCollector(nil, "bot", "org", slog.Default())
	for i := 0; i < 100; i++ {
		d := fc.startupDelay()
		if d < 0 || d >= fleetStatsStartupJitterMax {
			t.Fatalf("startupDelay() = %v, want within [0, %v)", d, fleetStatsStartupJitterMax)
		}
	}
}

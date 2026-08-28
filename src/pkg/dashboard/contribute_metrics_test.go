package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSampler feeds deterministic values into a rollup tick so the bucketing can
// be exercised without a live hub or a real hour passing.
type fakeSampler struct {
	queue  int
	fleet  int
	totals map[string]int
	now    time.Time
}

func (f *fakeSampler) sampleMetricsInputs() rollupSample {
	tot := make(map[string]int, len(f.totals))
	for k, v := range f.totals {
		tot[k] = v
	}
	return rollupSample{queueDepth: f.queue, fleetSize: f.fleet, userTotals: tot, now: f.now}
}

// TestMetricsStoreRoundTrip saves a store to the JSON file and reloads it,
// asserting the series survive the trip through /data (the PVC persistence path).
func TestMetricsStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	s := newMetricsStore(path, slog.Default())

	// One rollup with a couple of completions, a queue depth, and a fleet size.
	s.rollup(rollupSample{
		queueDepth: 7,
		fleetSize:  3,
		userTotals: map[string]int{}, // first tick seeds the baseline (no deltas)
		now:        time.Now(),
	})
	s.rollup(rollupSample{
		queueDepth: 9,
		fleetSize:  4,
		userTotals: map[string]int{"alice": 2, "bob": 1},
		now:        time.Now(),
	})

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected metrics file written, stat err: %v", err)
	}

	// Reload into a fresh store and compare.
	s2 := newMetricsStore(path, slog.Default())
	s2.load()
	snap := s2.snapshot()

	if got := len(snap.QueueDepth); got != 2 {
		t.Fatalf("queue_depth buckets = %d, want 2", got)
	}
	if snap.QueueDepth[1] != 9 || snap.FleetSize[1] != 4 {
		t.Fatalf("restored latest bucket wrong: queue=%v fleet=%v", snap.QueueDepth, snap.FleetSize)
	}
	// alice completed 2, bob 1 in the second bucket; the tasks_done total for that
	// bucket is 3.
	if snap.TasksDone[1] != 3 {
		t.Fatalf("tasks_done[1] = %d, want 3", snap.TasksDone[1])
	}
	if len(snap.PerUserDone["alice"]) == 0 || snap.PerUserDone["alice"][len(snap.PerUserDone["alice"])-1] != 2 {
		t.Fatalf("per_user_done[alice] wrong: %v", snap.PerUserDone["alice"])
	}
	if snap.Bucket != "hour" {
		t.Fatalf("bucket = %q, want hour", snap.Bucket)
	}
}

// TestMetricsRingRetention asserts every series caps at metricsRetentionBuckets
// (168) so a long-running spoke never grows the file without bound.
func TestMetricsRingRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	s := newMetricsStore(path, slog.Default())

	// Push well past the cap. Each tick raises alice's cumulative total by 1 so
	// every bucket after the first records a delta of 1.
	const ticks = metricsRetentionBuckets + 50
	for i := 0; i < ticks; i++ {
		s.rollup(rollupSample{
			queueDepth: i,
			fleetSize:  1,
			userTotals: map[string]int{"alice": i},
			now:        time.Now(),
		})
	}
	snap := s.snapshot()
	if len(snap.QueueDepth) != metricsRetentionBuckets {
		t.Fatalf("queue_depth len = %d, want cap %d", len(snap.QueueDepth), metricsRetentionBuckets)
	}
	if len(snap.TasksDone) != metricsRetentionBuckets {
		t.Fatalf("tasks_done len = %d, want cap %d", len(snap.TasksDone), metricsRetentionBuckets)
	}
	if len(snap.PerUserDone["alice"]) > metricsRetentionBuckets {
		t.Fatalf("per_user_done[alice] len = %d exceeds cap %d", len(snap.PerUserDone["alice"]), metricsRetentionBuckets)
	}
	// The most-recent queue_depth bucket must be the last value pushed.
	if snap.QueueDepth[len(snap.QueueDepth)-1] != ticks-1 {
		t.Fatalf("latest queue_depth = %d, want %d", snap.QueueDepth[len(snap.QueueDepth)-1], ticks-1)
	}
}

// TestMetricsStoreCorruptFile asserts a corrupt /data file yields an empty store
// with no panic — the resilience contract the tombstone/fleet-stats loaders keep.
func TestMetricsStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newMetricsStore(path, slog.Default())
	s.load() // must not panic
	snap := s.snapshot()
	if len(snap.QueueDepth) != 0 || len(snap.TasksDone) != 0 || len(snap.PerUserDone) != 0 {
		t.Fatalf("corrupt file should leave an empty store, got %+v", snap)
	}
}

// TestMetricsStoreMissingFile asserts a missing file is a clean no-op (first boot).
func TestMetricsStoreMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := newMetricsStore(path, slog.Default())
	s.load() // must not panic
	if len(s.snapshot().QueueDepth) != 0 {
		t.Fatalf("missing file should leave an empty store")
	}
}

// TestMetricsRollupBuckets drives the rollup goroutine with an injected short
// interval and asserts it buckets a tick correctly, keyed by the truncated hour.
func TestMetricsRollupBuckets(t *testing.T) {
	origInterval := metricsRollupInterval
	metricsRollupInterval = 5 * time.Millisecond
	t.Cleanup(func() { metricsRollupInterval = origInterval })

	path := filepath.Join(t.TempDir(), "metrics.json")
	store := newMetricsStore(path, slog.Default())

	sampler := &fakeSampler{
		queue:  5,
		fleet:  2,
		totals: map[string]int{"carol": 4},
		now:    time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { store.Start(ctx, sampler); close(done) }()

	// Wait for at least two ticks so the seed tick + a delta tick both land.
	deadline := time.After(2 * time.Second)
	for len(store.snapshot().QueueDepth) < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("rollup did not produce buckets in time")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done // Start must return promptly on ctx cancel (no goroutine leak).

	snap := store.snapshot()
	if snap.QueueDepth[0] != 5 || snap.FleetSize[0] != 2 {
		t.Fatalf("sampled point values wrong: queue=%v fleet=%v", snap.QueueDepth, snap.FleetSize)
	}
	// collected_at must be truncated to the hour boundary.
	ct, err := time.Parse(time.RFC3339, snap.CollectedAt)
	if err != nil {
		t.Fatalf("collected_at not RFC3339: %q (%v)", snap.CollectedAt, err)
	}
	if !ct.Equal(ct.Truncate(time.Hour)) {
		t.Fatalf("collected_at %v is not truncated to the hour", ct)
	}
}

// TestMetricsStoreSeedNoBackfill asserts the first tick after a (re)start does
// NOT book the whole historical cumulative total as one hour of completions.
func TestMetricsStoreSeedNoBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	s := newMetricsStore(path, slog.Default())

	// First tick sees a large pre-existing cumulative total. It must be treated as
	// the baseline (delta 0), not booked as 100 completions this hour.
	s.rollup(rollupSample{userTotals: map[string]int{"dave": 100}, now: time.Now()})
	snap := s.snapshot()
	if snap.TasksDone[0] != 0 {
		t.Fatalf("first tick booked %d tasks, want 0 (baseline seed)", snap.TasksDone[0])
	}
	// A subsequent +3 must be booked as exactly 3.
	s.rollup(rollupSample{userTotals: map[string]int{"dave": 103}, now: time.Now()})
	snap = s.snapshot()
	if snap.TasksDone[1] != 3 {
		t.Fatalf("second tick booked %d tasks, want 3", snap.TasksDone[1])
	}
}

// TestMetricsPersistShapeStable pins the on-disk JSON keys the client sparklines
// depend on, so a rename can't silently break the fetch contract.
func TestMetricsPersistShapeStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	s := newMetricsStore(path, slog.Default())
	s.rollup(rollupSample{queueDepth: 1, fleetSize: 1, userTotals: map[string]int{}, now: time.Now()})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"queue_depth", "tasks_done", "fleet_size", "per_user_done", "bucket"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("persisted metrics missing key %q", key)
		}
	}
}

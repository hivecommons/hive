package dashboard

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// The quadrant heartbeat sends TasksCompleted7d as a *int, where nil means "not
// measured" and a non-nil zero is a real reading of zero. These tests pin that
// distinction at the accessor, because collapsing the two is invisible at the
// wire level yet makes the hub score an unmeasured hive as a genuinely idle one.

// TestTasksCompleted7d_UnmeasuredStoreReportsNotOk is the negative case: a store
// that has neither rolled up nor restored anything has no measurement to report.
func TestTasksCompleted7d_UnmeasuredStoreReportsNotOk(t *testing.T) {
	s := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	total, ok := s.tasksCompleted7d()
	if ok {
		t.Errorf("virgin store must report ok=false, got ok=true total=%d", total)
	}
}

// TestTasksCompleted7d_ZeroIsAReading is the positive control that keeps the
// test above from passing for the wrong reason. A store that HAS rolled up but
// saw no completions must report ok=true with a total of zero — the real "no
// contributor finished anything" measurement, not a gap.
func TestTasksCompleted7d_ZeroIsAReading(t *testing.T) {
	s := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	// One tick with no contributor activity at all.
	s.rollup(rollupSample{queueDepth: 0, fleetSize: 0, userTotals: map[string]int{}, now: time.Now()})

	total, ok := s.tasksCompleted7d()
	if !ok {
		t.Fatal("a store that has rolled up must report ok=true even with zero completions")
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
}

// TestTasksCompleted7d_SumsRing asserts the accessor actually sums the ring
// rather than returning the latest bucket, so the positive control above cannot
// be satisfied by a stub that always returns zero.
func TestTasksCompleted7d_SumsRing(t *testing.T) {
	s := newMetricsStore(filepath.Join(t.TempDir(), "metrics.json"), slog.Default())

	// First tick seeds the baseline (no deltas booked), then two hours of work.
	s.rollup(rollupSample{userTotals: map[string]int{"ada": 0}, now: time.Now()})
	s.rollup(rollupSample{userTotals: map[string]int{"ada": 3}, now: time.Now()})
	s.rollup(rollupSample{userTotals: map[string]int{"ada": 8}, now: time.Now()})

	total, ok := s.tasksCompleted7d()
	if !ok {
		t.Fatal("ok=false after rollups")
	}
	// 3 in the first hour, 5 in the second — the seed tick books nothing.
	if total != 8 {
		t.Errorf("total = %d, want 8 (sum of the ring, not the last bucket)", total)
	}
}

// TestTasksCompleted7d_RestoredHistoryIsReportable covers the restart path:
// load() leaves seededTotals false, but buckets restored from the PVC are real
// past measurements, so the spoke must keep reporting them rather than going
// dark for the first hour after every restart.
func TestTasksCompleted7d_RestoredHistoryIsReportable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")

	src := newMetricsStore(path, slog.Default())
	src.rollup(rollupSample{userTotals: map[string]int{"ada": 0}, now: time.Now()})
	// rollup persists to path on every tick, so the file is already on disk.
	src.rollup(rollupSample{userTotals: map[string]int{"ada": 4}, now: time.Now()})

	restored := newMetricsStore(path, slog.Default())
	restored.load()
	if restored.seededTotals {
		t.Fatal("precondition: load() must not set seededTotals")
	}

	total, ok := restored.tasksCompleted7d()
	if !ok {
		t.Fatal("restored history must be reportable before the first rollup")
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
}

// TestServerTasksCompleted7d_NoStoreReportsNotOk asserts the Server-level
// accessor stays nil-safe AND does not lazily create the store: the heartbeat is
// a read-only observer, so merely watching must not conjure a store or touch the
// PVC as a side effect.
func TestServerTasksCompleted7d_NoStoreReportsNotOk(t *testing.T) {
	var s Server

	total, ok := s.TasksCompleted7d()
	if ok {
		t.Errorf("Server with no metrics store must report ok=false, got total=%d", total)
	}
	if s.contributeMetrics != nil {
		t.Error("accessor must not lazily build the metrics store")
	}
}

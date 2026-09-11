package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPerUserRingsAlignWithSharedTimeline is the bug half of #6543: before the
// fix, a per-user ring only grew on hours that contributor completed something,
// so index i of per_user_done[u] was NOT hour i of tasks_done. Anything reading
// those rings positionally — the "last 24 hours", the per-row sparklines — was
// quietly reporting a different window per contributor.
func TestPerUserRingsAlignWithSharedTimeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	// Tick 1 seeds the baselines. alice then works hours 2 and 5 only; bob works
	// hour 3 only. Both must still end up with a bucket for EVERY hour.
	aliceTotal, bobTotal := 0, 0
	for hour := 1; hour <= 6; hour++ {
		if hour == 2 || hour == 5 {
			aliceTotal++
		}
		if hour == 3 {
			bobTotal++
		}
		m.rollup(rollupSample{
			queueDepth: 0, fleetSize: 2,
			userTotals: map[string]int{"alice": aliceTotal, "bob": bobTotal},
			now:        time.Now(),
		})
	}

	snap := m.snapshot()
	want := len(snap.TasksDone)
	if want != 6 {
		t.Fatalf("tasks_done len = %d, want 6", want)
	}
	for user, ring := range snap.PerUserDone {
		if len(ring) != want {
			t.Fatalf("per_user_done[%s] len = %d, want %d (must share the timeline)", user, len(ring), want)
		}
	}
	// Index i is the same hour in every series, so the spikes land where the work
	// actually happened rather than bunched at the tail.
	if got := snap.PerUserDone["alice"]; got[1] != 1 || got[4] != 1 || got[0] != 0 || got[2] != 0 || got[3] != 0 || got[5] != 0 {
		t.Fatalf("alice ring = %v, want work at index 1 and 4 only", got)
	}
	if got := snap.PerUserDone["bob"]; got[2] != 1 || got[0] != 0 || got[1] != 0 || got[3] != 0 {
		t.Fatalf("bob ring = %v, want work at index 2 only", got)
	}
}

// TestNewContributorRingIsLeftPadded proves someone who registers mid-window
// lands at the TAIL of the shared timeline, not the head. Appending their first
// bucket at index 0 would date their first task to a week ago.
func TestNewContributorRingIsLeftPadded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	for i := 0; i < 5; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"veteran": i}, now: time.Now()})
	}
	// "latecomer" registers only now, at a cumulative zero, and finishes one task
	// in the following hour.
	m.rollup(rollupSample{userTotals: map[string]int{"veteran": 5, "latecomer": 0}, now: time.Now()})
	m.rollup(rollupSample{userTotals: map[string]int{"veteran": 6, "latecomer": 1}, now: time.Now()})

	snap := m.snapshot()
	ring := snap.PerUserDone["latecomer"]
	if len(ring) != len(snap.TasksDone) {
		t.Fatalf("latecomer ring len = %d, want %d", len(ring), len(snap.TasksDone))
	}
	for i := 0; i < len(ring)-1; i++ {
		if ring[i] != 0 {
			t.Fatalf("latecomer ring = %v, want leading zeros before they arrived", ring)
		}
	}
	if ring[len(ring)-1] != 1 {
		t.Fatalf("latecomer's newest bucket = %d, want the 1 task they just finished", ring[len(ring)-1])
	}
}

// TestIdleContributorKeepsFlatLine proves a registered contributor who finishes
// nothing keeps an honest zero-filled series rather than disappearing from the
// payload — a flat line is a true statement, an absent series is not.
func TestIdleContributorKeepsFlatLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	for i := 0; i < 4; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"idle": 0}, now: time.Now()})
	}
	snap := m.snapshot()
	ring, ok := snap.PerUserDone["idle"]
	if !ok {
		t.Fatal("registered-but-idle contributor dropped from per_user_done")
	}
	if len(ring) != len(snap.TasksDone) {
		t.Fatalf("idle ring len = %d, want %d", len(ring), len(snap.TasksDone))
	}
	for _, n := range ring {
		if n != 0 {
			t.Fatalf("idle ring = %v, want all zeros", ring)
		}
	}
}

// TestDepartedContributorPrunedWhenEmpty proves the zero-fill does not grow the
// map without bound: a contributor whose profile is gone AND who has nothing
// left inside the 7-day window is dropped rather than carried as zeros forever.
func TestDepartedContributorPrunedWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	m.rollup(rollupSample{userTotals: map[string]int{"gone": 0, "stays": 0}, now: time.Now()})
	m.rollup(rollupSample{userTotals: map[string]int{"gone": 0, "stays": 1}, now: time.Now()})
	// "gone" is no longer in the live profile list from here on.
	m.rollup(rollupSample{userTotals: map[string]int{"stays": 2}, now: time.Now()})

	snap := m.snapshot()
	if _, ok := snap.PerUserDone["gone"]; ok {
		t.Fatalf("all-zero departed contributor kept: %v", snap.PerUserDone["gone"])
	}
	if len(snap.PerUserDone["stays"]) != len(snap.TasksDone) {
		t.Fatalf("stays ring len = %d, want %d", len(snap.PerUserDone["stays"]), len(snap.TasksDone))
	}
}

// TestDepartedContributorWithWorkIsKeptAligned proves pruning only ever drops
// series that carry no information: a contributor who left but whose work is
// still inside the window keeps their history, zero-filled forward so it stays
// on the shared timeline.
func TestDepartedContributorWithWorkIsKeptAligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	m.rollup(rollupSample{userTotals: map[string]int{"left": 0}, now: time.Now()})
	m.rollup(rollupSample{userTotals: map[string]int{"left": 7}, now: time.Now()})
	m.rollup(rollupSample{userTotals: map[string]int{}, now: time.Now()})
	m.rollup(rollupSample{userTotals: map[string]int{}, now: time.Now()})

	snap := m.snapshot()
	ring, ok := snap.PerUserDone["left"]
	if !ok {
		t.Fatal("departed contributor with real work in the window was dropped")
	}
	if len(ring) != len(snap.TasksDone) {
		t.Fatalf("ring len = %d, want %d", len(ring), len(snap.TasksDone))
	}
	if ring[1] != 7 {
		t.Fatalf("ring = %v, want the 7 completions still at their own hour", ring)
	}
	if ring[len(ring)-1] != 0 {
		t.Fatalf("ring = %v, want zero-filled forward after they left", ring)
	}
}

// TestLegacyRaggedRingsResetOnLoad covers the upgrade path. A pre-#6543 file
// holds ragged per-user rings with no per-bucket timestamps, so there is no way
// to place their buckets on the timeline. Left-padding them would CLAIM all that
// work landed in the most recent N hours — precisely the wrong answer for a
// 24-hour rollup — so a misaligned ring is dropped instead of reinterpreted.
func TestLegacyRaggedRingsResetOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	legacy := metricsPersistShape{
		QueueDepth: make([]int, 10),
		TasksDone:  []int{1, 0, 2, 0, 0, 3, 0, 0, 0, 1},
		FleetSize:  make([]int, 10),
		PerUserDone: map[string][]int{
			"ragged":  {1, 2, 3},                      // 3 buckets against 10 hours
			"aligned": {0, 0, 1, 0, 0, 1, 0, 0, 0, 1}, // already on the timeline
		},
		Bucket: "hour",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := newMetricsStore(path, slog.Default())
	m.load()
	snap := m.snapshot()

	if len(snap.PerUserDone["ragged"]) != len(snap.TasksDone) {
		t.Fatalf("ragged ring len = %d, want %d", len(snap.PerUserDone["ragged"]), len(snap.TasksDone))
	}
	for _, n := range snap.PerUserDone["ragged"] {
		if n != 0 {
			t.Fatalf("ragged legacy ring = %v, want it zeroed rather than re-dated", snap.PerUserDone["ragged"])
		}
	}
	// An already-aligned ring is real history and must survive untouched.
	got := snap.PerUserDone["aligned"]
	if len(got) != 10 || got[2] != 1 || got[5] != 1 || got[9] != 1 {
		t.Fatalf("aligned ring = %v, want it kept as-is", got)
	}

	// And alignment holds from the next rollup on.
	m.rollup(rollupSample{userTotals: map[string]int{"ragged": 0, "aligned": 0}, now: time.Now()})
	snap = m.snapshot()
	for user, ring := range snap.PerUserDone {
		if len(ring) != len(snap.TasksDone) {
			t.Fatalf("after rollup per_user_done[%s] len = %d, want %d", user, len(ring), len(snap.TasksDone))
		}
	}
}

// TestPerUserRingsRespectRetentionCap proves zero-filling never lets a ring grow
// past the 168-bucket cap — a contributor now gets a bucket every hour, so an
// uncapped fill would grow the PVC file without bound.
func TestPerUserRingsRespectRetentionCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	for i := 0; i < metricsRetentionBuckets+40; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"steady": i}, now: time.Now()})
	}
	snap := m.snapshot()
	if len(snap.TasksDone) != metricsRetentionBuckets {
		t.Fatalf("tasks_done len = %d, want %d", len(snap.TasksDone), metricsRetentionBuckets)
	}
	if len(snap.PerUserDone["steady"]) != metricsRetentionBuckets {
		t.Fatalf("per_user_done[steady] len = %d, want the cap %d",
			len(snap.PerUserDone["steady"]), metricsRetentionBuckets)
	}
}

// TestUserRecentSumsTrailingWindow exercises the accessor the endpoint uses
// directly, including the short-history and unknown-contributor answers.
func TestUserRecentSumsTrailingWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	for i := 0; i < 30; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"w": i * 2}, now: time.Now()})
	}
	// Buckets 2..30 each carry a delta of 2; the trailing 24 sum to 48.
	sum, covered, known := m.userRecent("w", recentWindowBuckets)
	if !known {
		t.Fatal("known = false for a contributor with 30 buckets")
	}
	if covered != recentWindowBuckets {
		t.Fatalf("covered = %d, want %d", covered, recentWindowBuckets)
	}
	if sum != 48 {
		t.Fatalf("sum = %d, want 48", sum)
	}

	if _, _, known := m.userRecent("nobody", recentWindowBuckets); known {
		t.Fatal("known = true for a contributor with no series at all")
	}
	if _, _, known := m.userRecent("", recentWindowBuckets); known {
		t.Fatal("known = true for an empty username")
	}
	if sum, covered, _ := m.userRecent("w", 0); sum != 0 || covered != 0 {
		t.Fatalf("a zero-width window must sum nothing, got %d/%d", sum, covered)
	}
}

// TestUserRecentShortHistory proves covered reports the REAL depth of history so
// a caller can say "6h so far" instead of labelling six hours as a day.
func TestUserRecentShortHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	m := newMetricsStore(path, slog.Default())

	for i := 0; i < 6; i++ {
		m.rollup(rollupSample{userTotals: map[string]int{"w": i}, now: time.Now()})
	}
	sum, covered, known := m.userRecent("w", recentWindowBuckets)
	if !known || covered != 6 || sum != 5 {
		t.Fatalf("userRecent = sum %d / covered %d / known %v, want 5/6/true", sum, covered, known)
	}
}

// TestPadRingPlacement pins the padding helper's contract on its own: pad to the
// tail, trim the oldest, and never hand back a longer ring than asked for.
func TestPadRingPlacement(t *testing.T) {
	if got := padRing([]int{1, 2}, 5); len(got) != 5 || got[0] != 0 || got[2] != 0 || got[3] != 1 || got[4] != 2 {
		t.Fatalf("padRing([1 2], 5) = %v, want [0 0 0 1 2]", got)
	}
	if got := padRing([]int{1, 2, 3, 4}, 2); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("padRing([1 2 3 4], 2) = %v, want the recent tail [3 4]", got)
	}
	if got := padRing(nil, 3); len(got) != 3 || got[0] != 0 || got[2] != 0 {
		t.Fatalf("padRing(nil, 3) = %v, want [0 0 0]", got)
	}
	if got := padRing([]int{1}, 0); got != nil {
		t.Fatalf("padRing(_, 0) = %v, want nil", got)
	}
}

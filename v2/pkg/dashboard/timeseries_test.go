package dashboard

import "testing"

// tsEntry is a minimal entry type for exercising the generic timeSeries.
type tsEntry struct {
	T   int64
	Val int
}

func tsTsOf(e tsEntry) int64 { return e.T }

// TestTimeSeriesAppendAndSnapshot verifies appends land and snapshot copies.
func TestTimeSeriesAppendAndSnapshot(t *testing.T) {
	ts := newTimeSeries(10, 0, tsTsOf)
	if ts.len() != 0 {
		t.Fatalf("fresh len = %d, want 0", ts.len())
	}
	if !ts.append(tsEntry{T: 1, Val: 100}) {
		t.Fatal("first append should store")
	}
	got := ts.snapshot()
	if len(got) != 1 || got[0].Val != 100 {
		t.Fatalf("snapshot = %+v, want one entry val=100", got)
	}
	// Mutating the returned copy must not leak into the buffer.
	got[0].Val = 999
	if again := ts.snapshot(); again[0].Val != 100 {
		t.Errorf("snapshot is not a copy: got %d", again[0].Val)
	}
}

// TestTimeSeriesThrottle verifies the min-interval throttle suppresses a rapid
// second append and admits one past the interval.
func TestTimeSeriesThrottle(t *testing.T) {
	const interval = 300_000
	ts := newTimeSeries(10, interval, tsTsOf)

	if !ts.append(tsEntry{T: 1000}) {
		t.Fatal("first append should store")
	}
	// Within the interval → suppressed.
	if ts.append(tsEntry{T: 1000 + interval - 1}) {
		t.Error("append within interval should be suppressed")
	}
	if ts.len() != 1 {
		t.Fatalf("len = %d, want 1 after throttled append", ts.len())
	}
	// At/after the interval → admitted.
	if !ts.append(tsEntry{T: 1000 + interval}) {
		t.Error("append at interval boundary should be admitted")
	}
	if ts.len() != 2 {
		t.Fatalf("len = %d, want 2", ts.len())
	}
}

// TestTimeSeriesNoThrottle verifies a 0 interval admits every append (token
// history behavior).
func TestTimeSeriesNoThrottle(t *testing.T) {
	ts := newTimeSeries(10, 0, tsTsOf)
	for i := 0; i < 5; i++ {
		if !ts.append(tsEntry{T: int64(i)}) {
			t.Fatalf("append %d suppressed with no throttle", i)
		}
	}
	if ts.len() != 5 {
		t.Fatalf("len = %d, want 5", ts.len())
	}
}

// TestTimeSeriesCap verifies the ring buffer keeps only the most-recent tail.
func TestTimeSeriesCap(t *testing.T) {
	const capacity = 3
	ts := newTimeSeries(capacity, 0, tsTsOf)
	for i := 0; i < 6; i++ {
		ts.append(tsEntry{T: int64(i), Val: i})
	}
	got := ts.snapshot()
	if len(got) != capacity {
		t.Fatalf("len = %d, want cap %d", len(got), capacity)
	}
	// Oldest dropped: should hold the last 3 (Val 3,4,5).
	if got[0].Val != 3 || got[2].Val != 5 {
		t.Errorf("tail = %d..%d, want 3..5", got[0].Val, got[2].Val)
	}
}

// TestTimeSeriesSeedTrimsToTail verifies seed trims an over-cap slice to the
// most-recent tail and preserves ordering.
func TestTimeSeriesSeedTrimsToTail(t *testing.T) {
	const capacity = 4
	ts := newTimeSeries(capacity, 0, tsTsOf)

	over := make([]tsEntry, 10)
	for i := range over {
		over[i] = tsEntry{T: int64(i), Val: i}
	}
	ts.seed(over)

	got := ts.snapshot()
	if len(got) != capacity {
		t.Fatalf("seeded len = %d, want cap %d", len(got), capacity)
	}
	if got[0].Val != 6 || got[len(got)-1].Val != 9 {
		t.Errorf("kept tail = %d..%d, want 6..9", got[0].Val, got[len(got)-1].Val)
	}
}

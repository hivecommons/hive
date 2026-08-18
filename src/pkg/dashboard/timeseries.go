package dashboard

import (
	"sync"
	"time"
)

// timeSeries is a generic, concurrency-safe ring buffer of timestamped entries.
// It is the single mechanism behind every persisted sparkline history (token,
// fact, cost, trend): a bounded, most-recent-tail buffer with an optional
// minimum-interval throttle and copy-on-read semantics.
//
// It deliberately owns ONLY the storage mechanics (cap, throttle, mutex, copy).
// Each concrete history keeps its own typed entry struct and its own JSON
// contract, so migrating a bespoke buffer onto timeSeries is a behavior- and
// wire-preserving refactor: the on-disk files and /api/* endpoints are
// unchanged.
//
// T is the entry type. tsOf extracts an entry's timestamp (unix millis) so the
// buffer can enforce the min-interval throttle without T needing an interface.
type timeSeries[T any] struct {
	mu          sync.RWMutex
	entries     []T
	cap         int   // maximum retained entries (most-recent tail kept)
	minInterval int64 // minimum ms between appends; 0 disables throttling
	tsOf        func(T) int64
}

// newTimeSeries builds a ring buffer with the given cap and min-interval (ms;
// 0 disables the throttle). tsOf returns an entry's timestamp in unix millis.
func newTimeSeries[T any](capacity int, minIntervalMs int64, tsOf func(T) int64) *timeSeries[T] {
	return &timeSeries[T]{
		cap:         capacity,
		minInterval: minIntervalMs,
		tsOf:        tsOf,
	}
}

// append adds entry unless the throttle suppresses it (the entry's timestamp is
// less than minInterval after the last retained entry's). It reports whether the
// entry was stored. Callers that need the current wall clock should stamp the
// entry before calling; append uses tsOf(entry) for the throttle comparison.
func (ts *timeSeries[T]) append(entry T) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.minInterval > 0 && len(ts.entries) > 0 {
		last := ts.entries[len(ts.entries)-1]
		if ts.tsOf(entry)-ts.tsOf(last) < ts.minInterval {
			return false
		}
	}

	ts.entries = append(ts.entries, entry)
	if ts.cap > 0 && len(ts.entries) > ts.cap {
		ts.entries = ts.entries[len(ts.entries)-ts.cap:]
	}
	return true
}

// snapshot returns a copy of the retained entries so callers can't mutate the
// buffer's backing array.
func (ts *timeSeries[T]) snapshot() []T {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]T, len(ts.entries))
	copy(out, ts.entries)
	return out
}

// seed replaces the buffer contents (trimming to the most-recent cap tail),
// used to restore persisted history on startup.
func (ts *timeSeries[T]) seed(entries []T) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.cap > 0 && len(entries) > ts.cap {
		entries = entries[len(entries)-ts.cap:]
	}
	ts.entries = entries
}

// len reports the current number of retained entries.
func (ts *timeSeries[T]) len() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.entries)
}

// nowMillis is the wall clock used when stamping entries; a package var so
// tests could override it if needed.
func nowMillis() int64 { return time.Now().UnixMilli() }

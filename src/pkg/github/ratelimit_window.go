package github

import (
	"sync"
	"time"
)

// rateLimitWindow is the last ACCEPTED observation for one bucket.
type rateLimitWindow struct {
	limit int
	// remaining is the LOWEST remaining seen during this window — the whole
	// point of the clamp, since within a window the real value only falls.
	remaining int
	reset     time.Time
	// observedAt is when the reported remaining was actually observed. It stops
	// advancing while a reading is held, which is what makes a stale value
	// visibly stale on the dashboard instead of silently confident.
	observedAt time.Time
}

// rateLimitTracker holds one window per bucket ("core", "search", "graphql").
// The zero value is ready to use.
type rateLimitTracker struct {
	mu      sync.Mutex
	windows map[string]rateLimitWindow
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

func (t *rateLimitTracker) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// observe folds one raw reading into the tracked window and returns the value
// that should be reported.
//
// An empty entry (no limit, no remaining, no reset) means the API did not
// report that bucket at all; it is passed straight through and never tracked,
// so an absent bucket cannot pin a window.
func (t *rateLimitTracker) observe(bucket string, obs RateLimitEntry) RateLimitEntry {
	if obs.Limit == 0 && obs.Remaining == 0 && obs.Reset.IsZero() {
		return obs
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.windows == nil {
		t.windows = make(map[string]rateLimitWindow, 3)
	}

	now := t.clock()
	prev, tracked := t.windows[bucket]

	switch {
	case !tracked:
		// Nothing to compare against; the first reading establishes the window.

	case !now.Before(prev.reset):
		// The previous window's reset has actually passed, so this is a genuine
		// rollover and the budget really has been restored. Wall-clock expiry —
		// not a changed reset value — is what proves it.

	case obs.Reset.Equal(prev.reset):
		// Same window. The true remaining only falls within a window, so a
		// higher reading is not new information; it is the fresh-bucket
		// artifact. Keep the lowest seen.
		if obs.Remaining >= prev.remaining {
			return entryOf(prev)
		}

	default:
		// A different reset while the previous window is still open: a re-minted
		// token describing its own empty bucket, carrying its own later reset.
		// This is the exact reading that produced #5733; discard it.
		return entryOf(prev)
	}

	accepted := rateLimitWindow{
		limit:      obs.Limit,
		remaining:  obs.Remaining,
		reset:      obs.Reset,
		observedAt: now,
	}
	t.windows[bucket] = accepted
	return entryOf(accepted)
}

// adopt folds per-response evidence for a bucket (coreRateLimitEvidence: what
// the hive's own responses report, keyed by window) into the tracked window and
// returns the value that should be reported.
//
// A /rate_limit reading is a single sample, and the one a process takes right
// after it mints its token has twice described a different window from the
// one its traffic was spending: after the 2026-11-07 18:22Z restart it read
// 6613 (reset 19:13:07Z) while the installation was returning 403s on the
// window resetting at 18:54:37Z, and after the 2026-11-08 18:30:53Z restart it
// read 6945 (reset 19:21:44Z) against 3195 (reset 18:56:25Z). observe() then
// discarded every later reading until that wrong window's reset, so the card
// sat on one stale number for most of an hour. Per-response evidence comes from
// every response, so it decides which window is current:
//
//   - a different reset replaces the tracked window outright;
//   - the same reset keeps the lower remaining, as observe() does, and moves
//     observedAt forward when the evidence is at least as low.
func (t *rateLimitTracker) adopt(bucket string, ev RateLimitEntry) RateLimitEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.windows == nil {
		t.windows = make(map[string]rateLimitWindow, 3)
	}
	prev, tracked := t.windows[bucket]
	next := rateLimitWindow{
		limit:      ev.Limit,
		remaining:  ev.Remaining,
		reset:      ev.Reset,
		observedAt: ev.ObservedAt,
	}
	if next.limit <= 0 && tracked {
		next.limit = prev.limit
	}
	if tracked && ev.Reset.Equal(prev.reset) {
		switch {
		case ev.Remaining > prev.remaining:
			// The tracker already holds a lower reading for this window.
			return entryOf(prev)
		case ev.Remaining == prev.remaining && prev.observedAt.After(next.observedAt):
			next.observedAt = prev.observedAt
		}
	}
	t.windows[bucket] = next
	return entryOf(next)
}

func (t *rateLimitTracker) snapshot(bucket string) RateLimitEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.windows == nil {
		return RateLimitEntry{}
	}
	w, ok := t.windows[bucket]
	if !ok {
		return RateLimitEntry{}
	}
	return entryOf(w)
}

func entryOf(w rateLimitWindow) RateLimitEntry {
	return RateLimitEntry{
		Limit:      w.limit,
		Remaining:  w.remaining,
		Reset:      w.reset,
		ObservedAt: w.observedAt,
	}
}

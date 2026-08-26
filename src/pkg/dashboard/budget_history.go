package dashboard

import (
	"sync"
	"time"
)

// Per-budget-window history (kubestellar/hive#4298).
//
// The dashboard has always shown the CURRENT budget window: how much of the
// weekly limit this window has consumed, and when it rolls. That answers "am I
// about to run out", and it is the only question it can answer — the moment the
// window rolls, the previous window's number is gone.
//
// The question an administrator actually asks after two weeks away is the other
// one: "did the budget stop the hive while I was gone?" Nothing in the tree
// could answer it. The token sparkline records tokens over time but knows
// nothing about the limit or where the window boundaries fell, and the governor
// keeps only the open window's spend.
//
// This records one row per CLOSED window — when it ran, what the limit was,
// what was consumed, and whether it hit the limit — so "for every recent reset,
// how much of the budget had been used" becomes a lookup.
//
// COMPATIBILITY. A hive upgrading into this keeps no history it never
// collected: the list starts empty, every accessor tolerates that, and the
// first observation merely starts tracking. Nothing here can fail a status
// build — an unset limit, a zero window, and a governor that has not opened a
// window yet are ordinary inputs, not errors.

// budgetWindowMaxEntries caps the ring. With a weekly window, 26 entries is
// half a year — longer than an operator investigates, and trivially small on
// disk. A named constant so the retention is a deliberate number rather than an
// accident of the buffer size.
const budgetWindowMaxEntries = 26

// BudgetWindowEntry is one CLOSED budget window.
//
// Timestamps are epoch milliseconds, matching every other history series the
// dashboard persists, so the frontend parses them the same way.
type BudgetWindowEntry struct {
	// WindowStart and WindowEnd bound the window. WindowEnd is when the reset
	// happened, which is what an operator correlates against "the fortnight I
	// was away".
	WindowStart int64 `json:"windowStart"`
	WindowEnd   int64 `json:"windowEnd"`
	// Limit is the weekly token limit that was in force FOR THIS WINDOW.
	// Recorded per window rather than read live, because an operator who raised
	// the limit after a bad week must still see what the limit was when it bit.
	Limit int64 `json:"limit"`
	// Used is the tokens consumed — the high-water mark observed before the
	// roll, not a post-roll reading, which would always be ~0.
	Used int64 `json:"used"`
	// PctUsed is Used/Limit as a percentage; 0 when no limit was set.
	PctUsed float64 `json:"pctUsed"`
	// Exhausted records whether the window actually reached its limit. Stored
	// rather than recomputed, for the same reason Limit is: a later limit
	// change must not rewrite history and make a past window look fine.
	Exhausted bool `json:"exhausted"`
}

// budgetWindowTracker watches the open window and emits an entry when it rolls.
// A small leaf-locked struct: nothing here calls back into Server, so the mutex
// is taken and released inside one method.
type budgetWindowTracker struct {
	mu sync.Mutex
	// start/end bound the window being observed. Zero before the first
	// observation and whenever the governor reports no open window.
	start int64
	end   int64
	limit int64
	// peakUsed is the HIGH-WATER spend seen in the open window.
	//
	// High-water rather than last-seen because the roll is detected on the
	// observation AFTER it happened, by which time the live spend has already
	// reset toward zero. Reading it then would record every window as barely
	// used — precisely the wrong answer for the question this feature exists to
	// answer.
	peakUsed int64
	// exhausted latches for the same reason: the state is transient and the
	// roll is observed late.
	exhausted bool
	// closed holds completed windows, oldest first.
	closed []BudgetWindowEntry
}

// observe folds one status reading into the tracker, returning the entry for a
// window that just closed (nil when none did).
//
// windowStart/windowEnd are the open window's bounds in epoch milliseconds. A
// zero windowEnd means the governor has no window open (no limit configured),
// which closes any tracked window without starting a new one.
func (t *budgetWindowTracker) observe(windowStart, windowEnd, limit, used int64, exhausted bool) *BudgetWindowEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Same window still open: accumulate and stop.
	if windowEnd != 0 && windowEnd == t.end {
		if used > t.peakUsed {
			t.peakUsed = used
		}
		if exhausted {
			t.exhausted = true
		}
		if limit > 0 {
			t.limit = limit
		}
		return nil
	}

	// The window changed. Close the tracked one, if there was one.
	var rolled *BudgetWindowEntry
	if t.end != 0 {
		entry := BudgetWindowEntry{
			WindowStart: t.start,
			WindowEnd:   t.end,
			Limit:       t.limit,
			Used:        t.peakUsed,
			Exhausted:   t.exhausted,
		}
		if t.limit > 0 {
			const pctMultiplier = 100.0
			entry.PctUsed = float64(t.peakUsed) / float64(t.limit) * pctMultiplier
		}
		t.closed = append(t.closed, entry)
		if len(t.closed) > budgetWindowMaxEntries {
			t.closed = t.closed[len(t.closed)-budgetWindowMaxEntries:]
		}
		rolled = &entry
	}

	// Begin tracking the new window. A zero windowEnd leaves the tracker idle
	// rather than recording an empty row on every status build.
	if windowEnd == 0 {
		t.start, t.end, t.limit, t.peakUsed, t.exhausted = 0, 0, 0, 0, false
		return rolled
	}
	t.start, t.end, t.limit = windowStart, windowEnd, limit
	t.peakUsed, t.exhausted = used, exhausted
	return rolled
}

// snapshot returns a copy of the closed windows, newest FIRST — the order an
// operator reads them in, and the order the API serves.
func (t *budgetWindowTracker) snapshot() []BudgetWindowEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]BudgetWindowEntry, 0, len(t.closed))
	for i := len(t.closed) - 1; i >= 0; i-- {
		out = append(out, t.closed[i])
	}
	return out
}

// seed restores persisted history on startup. Entries arrive newest-first (the
// order snapshot emits and the API serves) and are stored oldest-first
// internally, so a save/load round trip preserves order. Over-long input is
// truncated to the newest entries rather than rejected: a file written by a
// build with a larger cap must not break startup.
func (t *budgetWindowTracker) seed(entries []BudgetWindowEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = t.closed[:0]
	for i := len(entries) - 1; i >= 0; i-- {
		t.closed = append(t.closed, entries[i])
	}
	if len(t.closed) > budgetWindowMaxEntries {
		t.closed = t.closed[len(t.closed)-budgetWindowMaxEntries:]
	}
}

// budgetWindowOnceKey guards lazy construction so a zero-value Server — as used
// in tests that do `&Server{}` — works without a constructor, exactly like
// initHistories does for the sparkline rings.
func (s *Server) budgetWindows() *budgetWindowTracker {
	s.budgetWindowOnce.Do(func() {
		s.budgetWindowHist = &budgetWindowTracker{}
	})
	return s.budgetWindowHist
}

// ObserveBudgetWindow folds the current status's budget into the per-window
// history, recording a row when the window has rolled since the last call.
// Called from UpdateStatus alongside the other history appenders.
//
// A nil status, or a status with no open window, is a no-op: a hive with
// budgeting off records nothing rather than a stream of empty rows.
func (s *Server) ObserveBudgetWindow(status *StatusPayload) {
	if status == nil {
		return
	}
	b := status.Budget

	// WindowEndsAt is empty unless a limit is set and a window is open — which
	// is exactly the "nothing to track" case. An unparseable timestamp is
	// treated the same way rather than aborting the status build.
	var startMs, endMs int64
	if b.WindowEndsAt != "" {
		if end, err := time.Parse(time.RFC3339, b.WindowEndsAt); err == nil {
			endMs = end.UnixMilli()
		}
	}
	if b.WindowStartsAt != "" {
		if start, err := time.Parse(time.RFC3339, b.WindowStartsAt); err == nil {
			startMs = start.UnixMilli()
		}
	}

	rolled := s.budgetWindows().observe(startMs, endMs, b.WeeklyBudget, b.Used, b.Exhausted)
	if rolled != nil && s.logger != nil {
		s.logger.Info("budget window rolled",
			"used", rolled.Used, "limit", rolled.Limit,
			"pct_used", rolled.PctUsed, "exhausted", rolled.Exhausted)
	}
}

// BudgetWindowHistory returns the closed budget windows, newest first. Empty on
// a hive that has not yet seen a window roll — ordinary, never an error.
func (s *Server) BudgetWindowHistory() []BudgetWindowEntry {
	return s.budgetWindows().snapshot()
}

// SeedBudgetWindowHistory restores persisted per-window history on startup.
func (s *Server) SeedBudgetWindowHistory(entries []BudgetWindowEntry) {
	s.budgetWindows().seed(entries)
}

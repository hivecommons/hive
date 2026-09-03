package dashboard

// Per-budget-window history (kubestellar/hive#4298) — the Server-side wiring.
// The tracker itself (collect.BudgetWindowTracker) moved to
// pkg/dashboard/collect in kubestellar/hive#5565 slice 2; this file keeps the
// Server accessors that fold status readings into it and serve its history.

import (
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
)

// budgetWindows guards lazy construction so a zero-value Server — as used in
// tests that do `&Server{}` — works without a constructor, exactly like
// initHistories does for the sparkline rings.
func (s *Server) budgetWindows() *collect.BudgetWindowTracker {
	s.budgetWindowOnce.Do(func() {
		s.budgetWindowHist = &collect.BudgetWindowTracker{}
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

	rolled := s.budgetWindows().Observe(startMs, endMs, b.WeeklyBudget, b.Used, b.Exhausted)
	if rolled != nil && s.logger != nil {
		s.logger.Info("budget window rolled",
			"used", rolled.Used, "limit", rolled.Limit,
			"pct_used", rolled.PctUsed, "exhausted", rolled.Exhausted)
	}
}

// BudgetWindowHistory returns the closed budget windows, newest first. Empty on
// a hive that has not yet seen a window roll — ordinary, never an error.
func (s *Server) BudgetWindowHistory() []collect.BudgetWindowEntry {
	return s.budgetWindows().Snapshot()
}

// SeedBudgetWindowHistory restores persisted per-window history on startup.
func (s *Server) SeedBudgetWindowHistory(entries []collect.BudgetWindowEntry) {
	s.budgetWindows().Seed(entries)
}

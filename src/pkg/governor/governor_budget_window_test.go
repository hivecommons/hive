package governor

import (
	"testing"
	"time"
)

// BudgetWindow feeds the quadrant heartbeat, which sends budget spend as a
// nilable pointer alongside its two bounds. ok=false is what keeps an unopened
// window from being reported as a window starting at the zero time, which the
// hub would score as an absurdly stale budget rather than as absent evidence.

// TestBudgetWindow_UnopenedReportsNotOk is the negative case: a governor whose
// budget window has never opened has no bounds to report.
func TestBudgetWindow_UnopenedReportsNotOk(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	start, end, ok := g.BudgetWindow()
	if ok {
		t.Errorf("unopened window must report ok=false, got start=%v end=%v", start, end)
	}
}

// TestBudgetWindow_OpenedReportsBounds is the positive control, so the test
// above cannot pass merely because BudgetWindow always returns false.
func TestBudgetWindow_OpenedReportsBounds(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	opened := time.Now().Add(-48 * time.Hour)
	g.SeedBudget(500, nil, nil, opened)

	start, end, ok := g.BudgetWindow()
	if !ok {
		t.Fatal("an opened window must report ok=true")
	}
	if !start.Equal(opened) {
		t.Errorf("start = %v, want %v (ResetAt is the window START, not the end)", start, opened)
	}
	// period_days is unset here, so the window falls back to the 7-day default.
	if want := opened.Add(BudgetWindowDuration); !end.Equal(want) {
		t.Errorf("end = %v, want %v", end, want)
	}
}

// TestBudgetWindow_HonoursConfiguredPeriod asserts the end is derived from the
// configured period_days rather than a hardcoded 7 days — the reason this
// accessor exists at all, since callers outside the package cannot see
// budgetWindowDuration's config fallback.
func TestBudgetWindow_HonoursConfiguredPeriod(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	cfg.Budget.PeriodDays = 3
	g := New(cfg, agents, testLogger())

	opened := time.Now()
	g.SeedBudget(0, nil, nil, opened)

	_, end, ok := g.BudgetWindow()
	if !ok {
		t.Fatal("ok=false after seeding a window")
	}
	if want := opened.Add(3 * 24 * time.Hour); !end.Equal(want) {
		t.Errorf("end = %v, want %v (must honour period_days=3)", end, want)
	}
}

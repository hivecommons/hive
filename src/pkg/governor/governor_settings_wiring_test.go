package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// #2323: governor.budget.period_days and .critical_pct were persisted in
// config but never read, so the window was pinned to 7 days and the warning
// to 90%. These tests lock in that both are now honored, and that an
// unset/zero value falls back to the defaults instead of 0.

// TestBudgetWindowDuration_ConfiguredPeriodDays verifies a configured
// period_days sets the effective window (30 days), not the hardcoded 7.
func TestBudgetWindowDuration_ConfiguredPeriodDays(t *testing.T) {
	cfg := config.GovernorConfig{Budget: config.BudgetConfig{PeriodDays: 30}}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())

	want := 30 * 24 * time.Hour
	if got := g.budgetWindowDuration(); got != want {
		t.Errorf("budgetWindowDuration() = %v, want %v", got, want)
	}
}

// TestBudgetWindowDuration_ZeroFallsBackToDefault verifies an unset
// period_days falls back to the 7-day default, not a 0-length window.
func TestBudgetWindowDuration_ZeroFallsBackToDefault(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, testLogger())

	if got := g.budgetWindowDuration(); got != BudgetWindowDuration {
		t.Errorf("zero period_days: budgetWindowDuration() = %v, want default %v", got, BudgetWindowDuration)
	}
	if BudgetWindowDuration != 7*24*time.Hour {
		t.Errorf("default window = %v, want 7 days", BudgetWindowDuration)
	}
}

// TestBudgetWindowRoll_HonorsConfiguredPeriod verifies the actual roll logic
// uses the configured window: a window opened 8 days ago must NOT roll when
// period_days is 30 (it would have rolled under the hardcoded 7-day window).
func TestBudgetWindowRoll_HonorsConfiguredPeriod(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	cfg.Budget.PeriodDays = 30
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	// Window opened 8 days ago, exhausted. Under the old hardcoded 7-day
	// window this would roll and clear suppression; under the configured
	// 30-day window it must still be within the window.
	g.SeedBudget(1500, nil, nil, time.Now().Add(-8*24*time.Hour))

	// Totals update at 8 days: must NOT roll (30-day window).
	g.UpdateBudgetFromTotals(1600, nil, nil)
	if due := dueSet(g); due["scanner"] {
		t.Error("30-day window must not roll after only 8 days; scanner should stay suppressed")
	}
	if !g.GetState().BudgetExhausted {
		t.Error("30-day window must not roll after only 8 days; budget should stay exhausted")
	}
}

// TestBudgetWindowRoll_ConfiguredPeriodRolls verifies the configured window
// still rolls once elapsed: a 30-day window opened 31 days ago rolls and
// clears suppression.
func TestBudgetWindowRoll_ConfiguredPeriodRolls(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	cfg.Budget.PeriodDays = 30
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now().Add(-31*24*time.Hour))

	// Totals update at 31 days: rolls the window, resetting spend to zero.
	g.UpdateBudgetFromTotals(5000, nil, nil)
	if due := dueSet(g); !due["scanner"] {
		t.Error("30-day window should roll after 31 days and clear suppression")
	}
	if g.GetState().BudgetExhausted {
		t.Error("state should clear exhausted after the configured window rolls")
	}
}

// TestBudgetWarnPct_ConfiguredThreshold verifies critical_pct=75 triggers the
// warning at 75% of the limit, not the hardcoded 90%.
func TestBudgetWarnPct_ConfiguredThreshold(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	cfg.Budget.CriticalPct = 75
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SetBudgetResetAt(time.Now()) // open window with baseline 0

	// 800 tokens = 80%: below the hardcoded 90% but above the configured 75%.
	trans := g.UpdateBudgetFromTotals(800, nil, nil)
	if !trans.WarnCrossed || !trans.WarnActive {
		t.Errorf("configured 75%% threshold should warn at 80%% spend, got %+v", trans)
	}
	if got := g.budgetWarnPct(); got != 75 {
		t.Errorf("budgetWarnPct() = %d, want 75", got)
	}
}

// TestBudgetWarnPct_ZeroFallsBackToDefault verifies an unset critical_pct
// falls back to the 90% default, not a 0% (warn-immediately) threshold.
func TestBudgetWarnPct_ZeroFallsBackToDefault(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	if got := g.budgetWarnPct(); got != BudgetWarnPct {
		t.Errorf("zero critical_pct: budgetWarnPct() = %d, want default %d", got, BudgetWarnPct)
	}

	g.SetBudgetLimit(1000)
	g.SetBudgetResetAt(time.Now())

	// 800 tokens = 80%: below the 90% default, so no warn should fire.
	trans := g.UpdateBudgetFromTotals(800, nil, nil)
	if trans.WarnCrossed || trans.WarnActive {
		t.Errorf("default 90%% threshold must not warn at 80%% spend, got %+v", trans)
	}
	// 950 tokens = 95%: above 90%, warn should fire.
	trans = g.UpdateBudgetFromTotals(950, nil, nil)
	if !trans.WarnCrossed || !trans.WarnActive {
		t.Errorf("default 90%% threshold should warn at 95%% spend, got %+v", trans)
	}
}

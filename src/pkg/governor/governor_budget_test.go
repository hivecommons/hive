package governor

import (
	"testing"
	"time"
)

// dueSet runs one eval and returns the set of agents due for a kick.
func dueSet(g *Governor) map[string]bool {
	due := g.Evaluate(0, 0, 0, 0)
	set := make(map[string]bool, len(due))
	for _, name := range due {
		set[name] = true
	}
	return set
}

func TestBudgetExhausted_SuppressesKicks(t *testing.T) {
	cfg, agents := standardConfig("scanner", "outreach")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1000, nil, nil, time.Now())

	due := dueSet(g)
	if due["scanner"] || due["outreach"] {
		t.Errorf("exhausted budget should suppress kicks, got due=%v", due)
	}
	if !g.GetState().BudgetExhausted {
		t.Error("state should report budget exhausted")
	}
}

func TestBudgetExhausted_ExemptAgentsStillDue(t *testing.T) {
	cfg, agents := standardConfig("scanner", "outreach")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now())
	g.SetBudgetIgnored([]string{"outreach"})

	due := dueSet(g)
	if due["scanner"] {
		t.Error("non-exempt scanner should be suppressed")
	}
	if !due["outreach"] {
		t.Error("exempt outreach should still be due")
	}
}

func TestBudgetZeroLimit_NeverSuppresses(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	// Massive spend but no limit: budgeting is entirely off.
	g.SeedBudget(1_000_000_000, nil, nil, time.Now())

	due := dueSet(g)
	if !due["scanner"] {
		t.Error("zero limit must never suppress kicks")
	}
	if g.GetState().BudgetExhausted {
		t.Error("zero limit must never report exhausted")
	}
}

func TestBudgetTransitions_OneShotPerWindow(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SetBudgetResetAt(time.Now()) // open the window with baseline 0

	trans := g.UpdateBudgetFromTotals(950, nil, nil)
	if !trans.WarnCrossed || !trans.WarnActive {
		t.Errorf("expected warn crossing at 95%%, got %+v", trans)
	}
	if trans.ExhaustedCrossed || trans.ExhaustedActive {
		t.Errorf("unexpected exhausted state below limit: %+v", trans)
	}

	trans = g.UpdateBudgetFromTotals(1200, nil, nil)
	if trans.WarnCrossed {
		t.Error("warn crossing must fire only once per window")
	}
	if !trans.ExhaustedCrossed || !trans.ExhaustedActive {
		t.Errorf("expected exhausted crossing over limit, got %+v", trans)
	}

	trans = g.UpdateBudgetFromTotals(1300, nil, nil)
	if trans.WarnCrossed || trans.ExhaustedCrossed {
		t.Errorf("crossings must not repeat within a window: %+v", trans)
	}
	if !trans.WarnActive || !trans.ExhaustedActive {
		t.Errorf("active levels should keep reporting: %+v", trans)
	}
}

func TestBudgetTransitions_ZeroLimitNeverAlerts(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.UpdateBudgetFromTotals(0, nil, nil) // opens the window at baseline 0
	trans := g.UpdateBudgetFromTotals(1_000_000, nil, nil)
	if trans.WarnCrossed || trans.ExhaustedCrossed || trans.WarnActive || trans.ExhaustedActive {
		t.Errorf("zero limit must never trigger budget alerts: %+v", trans)
	}
}

func TestBudgetWindowRoll_ClearsSuppression(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	// Exhausted window that expired more than a week ago.
	g.SeedBudget(1500, nil, nil, time.Now().Add(-BudgetWindowDuration-time.Hour))

	if due := dueSet(g); due["scanner"] {
		t.Fatal("scanner should be suppressed before the window rolls")
	}

	// Next totals update rolls the window and resets spend to zero.
	g.UpdateBudgetFromTotals(5000, nil, nil)

	due := dueSet(g)
	if !due["scanner"] {
		t.Error("window roll should clear kick suppression")
	}
	if g.GetState().BudgetExhausted {
		t.Error("state should clear exhausted after window roll")
	}
}

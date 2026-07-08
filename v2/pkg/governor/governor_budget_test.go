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

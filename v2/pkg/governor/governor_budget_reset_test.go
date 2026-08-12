package governor

import (
	"testing"
	"time"
)

// ResetBudgetWindow must reopen the kick gate immediately: spend re-anchors
// at zero, the exhausted flag clears, and suppressed agents are due again on
// the very next eval — without waiting out the configured period.
func TestResetBudgetWindow_ReopensKickGate(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now())
	if due := dueSet(g); due["scanner"] {
		t.Fatal("precondition: exhausted budget should suppress kicks")
	}
	if !g.GetState().BudgetExhausted {
		t.Fatal("precondition: state should report budget exhausted")
	}

	g.ResetBudgetWindow()

	if b := g.GetBudget(); b.CurrentSpend != 0 {
		t.Errorf("spend after reset = %d, want 0", b.CurrentSpend)
	}
	if g.GetState().BudgetExhausted {
		t.Error("exhausted flag should clear on reset")
	}
	if due := dueSet(g); !due["scanner"] {
		t.Error("kicks should resume immediately after window reset")
	}
}

// The reset must fold the old window's spend into the baseline so the next
// collector scan (which reports LIFETIME totals) counts only tokens consumed
// after the reset — not the whole history again.
func TestResetBudgetWindow_RebaselinesLifetimeTotals(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())
	g.SetBudgetLimit(1000)

	// Open a window at baseline 0, then observe 5000 lifetime tokens: all
	// 5000 are in-window spend.
	g.UpdateBudgetFromTotals(0, nil, nil)
	g.UpdateBudgetFromTotals(5000, nil, nil)
	if b := g.GetBudget(); b.CurrentSpend != 5000 {
		t.Fatalf("precondition: in-window spend = %d, want 5000", b.CurrentSpend)
	}

	g.ResetBudgetWindow()

	// Lifetime grows to 5200 — only the 200 consumed after the reset may
	// count against the fresh window.
	g.UpdateBudgetFromTotals(5200, nil, nil)
	if b := g.GetBudget(); b.CurrentSpend != 200 {
		t.Errorf("post-reset spend = %d, want 200", b.CurrentSpend)
	}
}

// The once-per-window alert latches must rearm on reset, so the next
// exhaustion of the fresh window alerts again instead of staying silent.
func TestResetBudgetWindow_RearmsAlertLatches(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())
	g.SetBudgetLimit(100)

	g.UpdateBudgetFromTotals(0, nil, nil)
	trans := g.UpdateBudgetFromTotals(150, nil, nil)
	if !trans.ExhaustedCrossed {
		t.Fatal("precondition: first exhaustion should cross the alert latch")
	}

	g.ResetBudgetWindow()

	trans = g.UpdateBudgetFromTotals(300, nil, nil)
	if !trans.ExhaustedCrossed {
		t.Error("exhaustion of the fresh window should alert again after reset")
	}
}

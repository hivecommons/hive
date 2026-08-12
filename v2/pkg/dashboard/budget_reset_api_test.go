package dashboard

import (
	"net/http"
	"testing"
	"time"
)

// POST /api/config/governor/budget/reset must open a fresh budget window:
// spend re-anchors at zero (old spend folded into the baseline) so suppressed
// kicks resume while budget enforcement stays on.
func TestGovernorBudgetResetEndpoint(t *testing.T) {
	s, deps := apiServer(t)
	deps.Governor.SetBudgetLimit(100)
	deps.Governor.SeedBudget(500, nil, nil, time.Now())

	rec := doPost(s, "/api/config/governor/budget/reset", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
	}

	b := deps.Governor.GetBudget()
	if b.CurrentSpend != 0 {
		t.Errorf("spend = %d, want 0 after reset", b.CurrentSpend)
	}
	if b.WindowBaseline != 500 {
		t.Errorf("baseline = %d, want 500 (old window's spend folded in)", b.WindowBaseline)
	}
	if b.ResetAt.IsZero() {
		t.Error("ResetAt should be stamped by the reset")
	}
}

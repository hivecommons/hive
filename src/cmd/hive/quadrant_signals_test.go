package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
)

// ── Quadrant heartbeat signals: nil means "not reported" ──────────────────────
//
// The hub's scorer reads a nil pointer as absent evidence and a non-nil zero as
// a genuine low score. The heartbeat collector therefore gates each signal on
// its source having actually produced a measurement. These tests pin the gates
// at the sources, because a regression here is invisible on the wire: the hub
// would simply start scoring un-booted spokes as healthy-and-idle.

func quadrantTestGovernor(t *testing.T) *governor.Governor {
	t.Helper()
	return governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.New(slog.DiscardHandler))
}

// TestQuadrant_GovernorSignalsGatedOnLastEval is the negative case: before the
// first eval, BudgetExhausted=false and SLAViolations=0 are struct defaults
// rather than readings, and LastEval is the only thing that says so.
func TestQuadrant_GovernorSignalsGatedOnLastEval(t *testing.T) {
	g := quadrantTestGovernor(t)

	if st := g.GetState(); !st.LastEval.IsZero() {
		t.Fatalf("a fresh governor must have a zero LastEval, got %v", st.LastEval)
	}
}

// TestQuadrant_GovernorSignalsReportableAfterSeed is the positive control: a
// restart restores LastEval from persisted state (SeedLastEval), so a rebooted
// spoke keeps reporting instead of going dark until its next eval.
func TestQuadrant_GovernorSignalsReportableAfterSeed(t *testing.T) {
	g := quadrantTestGovernor(t)
	g.SeedLastEval(time.Now().Add(-5 * time.Minute))
	g.SeedQueueState(3, 1, 2, 4)

	st := g.GetState()
	if st.LastEval.IsZero() {
		t.Fatal("LastEval must survive SeedLastEval")
	}
	if st.SLAViolations != 4 {
		t.Errorf("SLAViolations = %d, want 4", st.SLAViolations)
	}
}

// TestQuadrant_AwaitingReviewUnavailableBelowL5 is the negative case for
// planning: below ACMM L5 the subsystem does not exist, so AwaitingReview's zero
// is structural. Reporting it would tell the hub "no plans are blocked on a
// human" about a hive that cannot have plans at all.
func TestQuadrant_AwaitingReviewUnavailableBelowL5(t *testing.T) {
	stores := map[string]*beads.Store{}

	for _, level := range []int{0, 1, 2, 3, 4} {
		if p := dashboard.BuildPlanning(stores, false, level); p.Available {
			t.Errorf("ACMM L%d: planning must be unavailable", level)
		}
	}
}

// TestQuadrant_AwaitingReviewAvailableAtL5 is the positive control, so the test
// above cannot pass because BuildPlanning always reports unavailable. At L5 the
// subsystem exists and a zero becomes a real measurement.
func TestQuadrant_AwaitingReviewAvailableAtL5(t *testing.T) {
	p := dashboard.BuildPlanning(map[string]*beads.Store{}, false, 5)
	if !p.Available {
		t.Fatal("ACMM L5: planning must be available")
	}
	if p.AwaitingReview != 0 {
		t.Errorf("AwaitingReview = %d, want 0 with no stores", p.AwaitingReview)
	}
}

// TestQuadrant_BudgetSpendTravelsWithItsWindow pins the coupling: spend is
// uninterpretable without bounds (zero equally means "the window just rolled"
// and "nothing was consumed"), so the collector emits all three or none.
func TestQuadrant_BudgetSpendTravelsWithItsWindow(t *testing.T) {
	g := quadrantTestGovernor(t)

	if _, _, ok := g.BudgetWindow(); ok {
		t.Fatal("no window has opened yet: spend must not be reportable")
	}

	// Positive control: once a window opens, all three become reportable.
	g.SeedBudget(1234, nil, nil, time.Now().Add(-time.Hour))
	start, end, ok := g.BudgetWindow()
	if !ok {
		t.Fatal("an opened window must be reportable")
	}
	if !end.After(start) {
		t.Errorf("window end %v must be after start %v", end, start)
	}
	if spend := g.GetBudget().CurrentSpend; spend != 1234 {
		t.Errorf("CurrentSpend = %d, want 1234", spend)
	}
}

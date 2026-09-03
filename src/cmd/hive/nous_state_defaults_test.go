package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard"
)

// loadNousState builds the dashboard's nous panel state from optional on-disk
// artifacts. Whatever is (or is not) on disk, the projected Status map must be
// internally consistent: the legacy and new key spellings must agree, the
// counts must match the loaded slices, and the phase must follow the ledger —
// a Status map that disagrees with the state it summarizes renders a lying
// panel.
func TestLoadNousStateStatusConsistency(t *testing.T) {
	state := loadNousState(restoreTestLogger())

	if state.Mode != "observe" || state.Scope != "governor" {
		t.Errorf("mode/scope = %q/%q, want observe/governor", state.Mode, state.Scope)
	}

	wantPhase := "collecting"
	if len(state.Ledger) > 0 {
		wantPhase = "observing"
	}
	if state.Phase != wantPhase {
		t.Errorf("phase = %q with %d ledger iterations, want %q", state.Phase, len(state.Ledger), wantPhase)
	}

	// Legacy and new key spellings must never diverge.
	if state.Status["snapshots"] != state.Status["snapshotCount"] {
		t.Errorf("snapshots %v != snapshotCount %v", state.Status["snapshots"], state.Status["snapshotCount"])
	}
	if state.Status["principles"] != state.Status["principleCount"] {
		t.Errorf("principles %v != principleCount %v", state.Status["principles"], state.Status["principleCount"])
	}

	// Counts must reflect the loaded state, not a stale or hardcoded value.
	if got, ok := state.Status["iterations"].(int); !ok || got != len(state.Ledger) {
		t.Errorf("iterations = %v, want %d", state.Status["iterations"], len(state.Ledger))
	}
	if got, ok := state.Status["principles"].(int); !ok || got != len(state.Principles) {
		t.Errorf("principles = %v, want %d", state.Status["principles"], len(state.Principles))
	}

	// The Status map's own mode/scope/phase must mirror the struct fields.
	if state.Status["mode"] != state.Mode || state.Status["scope"] != state.Scope || state.Status["phase"] != state.Phase {
		t.Errorf("Status mode/scope/phase %v/%v/%v diverge from state %q/%q/%q",
			state.Status["mode"], state.Status["scope"], state.Status["phase"], state.Mode, state.Scope, state.Phase)
	}

	// The baseline target rides from the dashboard package's single constant.
	if state.Status["baseline_target"] != dashboard.NousBaselineTarget {
		t.Errorf("baseline_target = %v, want %d", state.Status["baseline_target"], dashboard.NousBaselineTarget)
	}
}

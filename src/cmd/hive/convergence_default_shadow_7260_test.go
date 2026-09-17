package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestConvergenceDefault_ProducesEvidenceWithoutWithholdingAnything is the
// load-bearing guard for #7260.
//
// The default moved from "off" to "shadow", and that is only defensible if
// BOTH halves hold at the real applicator, on the production observer stack:
//
//  1. Nothing is withheld. A hive that never touched the knob must dispatch
//     exactly what it dispatched before — same result pointer, same
//     population. If this half ever breaks, the new default silently starts
//     gating real work on every hive in the fleet.
//  2. Evidence is actually produced. Recording a soak row is the entire
//     reason to prefer shadow over off; a default that withheld nothing AND
//     recorded nothing would just be "off" wearing a different name, and the
//     documented promote-on-your-own-evidence rollout would stay impossible.
//
// Asserting both together is what makes this a guard rather than a pin: it
// fails if someone "fixes" the default back to inert, and it fails if someone
// makes the default enforce.
func TestConvergenceDefault_ProducesEvidenceWithoutWithholdingAnything(t *testing.T) {
	// Unset, not "off" — the whole point is what an untouched hive does.
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A config with no convergence block at all: a hive upgraded into this
	// build that has never opened the settings tab.
	cfg := &config.Config{}
	if got := cfg.ConvergenceMode(); got != config.ConvergenceModeShadow {
		t.Fatalf("an untouched config resolved %q, want shadow", got)
	}

	got := applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)

	// Half 1: dispatch-identical to off.
	if got != actionable {
		t.Fatal("the default must return the raw actionable result itself — no candidate may be withheld")
	}
	if len(got.Issues.Items) != 2 {
		t.Fatalf("the default must not touch the population, got %+v", got.Issues.Items)
	}

	// Half 2: the evidence the rollout procedure depends on exists.
	hist := srv.ConvergenceSoakHistory()
	if len(hist) != 1 {
		t.Fatalf("the default must record one soak row per pass, got %d", len(hist))
	}
	row := hist[0]
	if row.Mode != config.ConvergenceModeShadow {
		t.Fatalf("soak row mode = %q, want shadow", row.Mode)
	}
	if row.Enforced {
		t.Fatal("the default must never record an enforced pass")
	}
	// The fixture has one genuinely blocked candidate, so this pass is exactly
	// the case an operator needs to see before promoting to enforce. A row
	// that could not tell "would differ" from "would not" carries no signal.
	if !row.WouldDiffer {
		t.Fatalf("soak row must report that enforcement would have differed: %+v", row)
	}
	if row.RawIssues != 2 || row.Admitted != 1 || row.Blocked != 1 {
		t.Fatalf("soak row counts = %+v, want raw=2 admitted=1 blocked=1", row)
	}
}

// TestConvergenceDefault_ExplicitOffStillShortCircuits pins the other
// direction: moving the DEFAULT must not remove the posture.
//
// "off" is the rollback for an operator who dislikes the new behaviour, and it
// is the control arm #4263's fixed-commit A/B comparison is built on. An
// operator who wrote "off" keeps getting off, with no migration and no
// telemetry — the code path stays entirely inert.
func TestConvergenceDefault_ExplicitOffStillShortCircuits(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeOff}}
	if got := cfg.ConvergenceMode(); got != config.ConvergenceModeOff {
		t.Fatalf("an explicit off resolved %q, want off", got)
	}

	got := applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)
	if got != actionable {
		t.Fatal("mode=off must return the raw actionable result itself")
	}
	if n := len(srv.ConvergenceSoakHistory()); n != 0 {
		t.Fatalf("mode=off must stay inert and record nothing, got %d soak rows", n)
	}
}

// TestConvergenceDefault_UnparseableModeStillFailsSafe guards the distinction
// #7260 had to introduce inside the resolver.
//
// Unset means "never chose" and takes the new default. A non-empty value that
// names no known mode means "chose something this build cannot honour" — a
// typo, or a mode from a newer build during a rollback — and must still take
// the "off" fail-safe. If these two collapse into one another, a typo starts
// silently selecting a posture the operator did not ask for, which is exactly
// the property the original default-off design was protecting.
func TestConvergenceDefault_UnparseableModeStillFailsSafe(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, raw := range []string{"shadowy", "enforce-all", "true", "1", "observe"} {
		srv := kickTestDashboard(t)
		cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: raw}}
		if got := cfg.ConvergenceMode(); got != config.ConvergenceModeOff {
			t.Fatalf("unparseable mode %q resolved %q, want off", raw, got)
		}
		applyConvergenceKickAdmission(cfg, srv, kickTestActionable(), nil, logger)
		if n := len(srv.ConvergenceSoakHistory()); n != 0 {
			t.Fatalf("unparseable mode %q must stay inert, got %d soak rows", raw, n)
		}
	}
}

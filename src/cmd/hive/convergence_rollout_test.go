package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ── #4263: enforce mode + captured pair + transition logging + soak rows ──────
//
// The rollout contract rows that #4263 adds on top of #4247's shadow seam:
// enforce gates ONLY the enrolled internal-dispatch path using the same
// decision shadow computes, off/shadow return the exact baseline population,
// every pass records one bounded soak row, and a mode transition is logged
// exactly once on the crossing.

// TestApplyConvergenceKickAdmission_OffReturnsBaselineAndRecordsNothing pins
// the default-off guarantee at the applicator's return value: the SAME
// pointer, zero soak rows, zero admission influence.
func TestApplyConvergenceKickAdmission_OffReturnsBaselineAndRecordsNothing(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got := applyConvergenceKickAdmission(&config.Config{}, srv, actionable, nil, logger)
	if got != actionable {
		t.Fatal("mode=off must return the raw actionable result itself")
	}
	if len(srv.ConvergenceSoakHistory()) != 0 {
		t.Fatal("mode=off must not record soak telemetry — the code path is inert")
	}
}

// TestApplyConvergenceKickAdmission_ShadowComputesButNeverBlocks: shadow
// returns the exact baseline population while the soak row records what
// enforcement would have changed (would_differ=true, enforced=false).
func TestApplyConvergenceKickAdmission_ShadowComputesButNeverBlocks(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got := applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)
	if got != actionable {
		t.Fatal("mode=shadow must return the raw actionable result itself (baseline disposition)")
	}
	if len(got.Issues.Items) != 2 {
		t.Fatalf("shadow must not touch the population: %+v", got.Issues.Items)
	}

	hist := srv.ConvergenceSoakHistory()
	if len(hist) != 1 {
		t.Fatalf("shadow must record exactly one soak row per pass, got %d", len(hist))
	}
	row := hist[0]
	if row.Mode != "shadow" || row.Enforced || !row.WouldDiffer {
		t.Fatalf("shadow row = %+v, want mode=shadow enforced=false would_differ=true", row)
	}
	if row.RawIssues != 2 || row.Admitted != 1 || row.Blocked != 1 || row.Unknown != 0 {
		t.Fatalf("shadow row counts = %+v, want raw=2 admitted=1 blocked=1 unknown=0", row)
	}
	if row.Commit == "" || row.Generation == 0 || row.EnrolledPath == "" {
		t.Fatalf("soak row missing commit/generation/path attribution: %+v", row)
	}
}

// TestApplyConvergenceKickAdmission_EnforceGatesOnlyEnrolledPath: the blocked
// candidate is absent from the returned (scheduled/cached) issue payload while
// the unrelated ready control remains present, the raw input is untouched, and
// the soak row records enforced=true.
func TestApplyConvergenceKickAdmission_EnforceGatesOnlyEnrolledPath(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeEnforce}}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	got := applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)
	if got == actionable {
		t.Fatal("enforce must return a gated projection, not the raw result")
	}
	if len(got.Issues.Items) != 1 || got.Issues.Items[0].Number != 700 {
		t.Fatalf("enforce must withhold the blocked candidate and keep ready work: %+v", got.Issues.Items)
	}
	if got.Issues.Count != 1 {
		t.Fatalf("gated count = %d, want 1", got.Issues.Count)
	}
	// The RAW population stays authoritative for every non-enrolled consumer.
	if len(actionable.Issues.Items) != 2 || actionable.Issues.Count != 2 {
		t.Fatalf("enforce must never mutate the raw actionable result: %+v", actionable.Issues)
	}
	if !strings.Contains(buf.String(), "WITHHELD") || strings.Contains(buf.String(), "not enforced") {
		t.Fatalf("enforce must log an actual withholding: %s", buf.String())
	}

	hist := srv.ConvergenceSoakHistory()
	if len(hist) != 1 || !hist[0].Enforced || !hist[0].WouldDiffer || hist[0].Blocked != 1 {
		t.Fatalf("enforce soak row = %+v, want enforced=true would_differ=true blocked=1", hist)
	}
}

// TestApplyConvergenceKickAdmission_EnforceToOffRestoresBaseline: flipping
// enforce back to off restores the pre-enrollment population on the very next
// pass — no restart, no queue surgery, no stored-state migration.
func TestApplyConvergenceKickAdmission_EnforceToOffRestoresBaseline(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	enforceCfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeEnforce}}
	gated := applyConvergenceKickAdmission(enforceCfg, srv, kickTestActionable(), nil, logger)
	if len(gated.Issues.Items) != 1 {
		t.Fatalf("enforce pass must gate: %+v", gated.Issues.Items)
	}

	offCfg := &config.Config{}
	baseline := kickTestActionable()
	restored := applyConvergenceKickAdmission(offCfg, srv, baseline, nil, logger)
	if restored != baseline || len(restored.Issues.Items) != 2 {
		t.Fatalf("the next off pass must restore the exact baseline: %+v", restored.Issues.Items)
	}
}

// TestApplyConvergenceKickAdmission_TransitionLoggedOnceOnCrossing: a mode
// change is logged on the crossing and never repeated on later identical
// passes — the #4305 transition-only discipline.
func TestApplyConvergenceKickAdmission_TransitionLoggedOnceOnCrossing(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	offCfg := &config.Config{}
	shadowCfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}

	// Boot pass: first capture is NOT a transition.
	applyConvergenceKickAdmission(offCfg, srv, kickTestActionable(), nil, logger)
	if strings.Contains(buf.String(), "convergence rollout mode changed") {
		t.Fatal("the first captured pass after boot must not report a transition")
	}

	// The flip: exactly one transition line.
	applyConvergenceKickAdmission(shadowCfg, srv, kickTestActionable(), nil, logger)
	if got := strings.Count(buf.String(), "convergence rollout mode changed"); got != 1 {
		t.Fatalf("transition logged %d times, want exactly 1: %s", got, buf.String())
	}

	// Steady state: no repeat.
	applyConvergenceKickAdmission(shadowCfg, srv, kickTestActionable(), nil, logger)
	if got := strings.Count(buf.String(), "convergence rollout mode changed"); got != 1 {
		t.Fatalf("an unchanged mode must not re-log the transition, got %d", got)
	}
}

// TestApplyConvergenceKickAdmission_OnePairPerPass: every soak row a pass
// records carries the pair captured at the start of that pass; a later flip
// produces a new generation on the NEXT pass, never a mixed sweep.
func TestApplyConvergenceKickAdmission_OnePairPerPass(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	shadowCfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}
	enforceCfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeEnforce}}

	applyConvergenceKickAdmission(shadowCfg, srv, kickTestActionable(), nil, logger)
	applyConvergenceKickAdmission(enforceCfg, srv, kickTestActionable(), nil, logger)

	hist := srv.ConvergenceSoakHistory() // newest first
	if len(hist) != 2 {
		t.Fatalf("want 2 soak rows, got %d", len(hist))
	}
	if hist[1].Mode != "shadow" || hist[0].Mode != "enforce" {
		t.Fatalf("rows must carry their own pass's mode: %+v", hist)
	}
	if hist[0].Generation <= hist[1].Generation {
		t.Fatalf("the flip must advance the generation: %d then %d", hist[1].Generation, hist[0].Generation)
	}
}

// TestApplyConvergenceKickAdmission_EnforcePreservesNonIssuePayloads: PRs and
// holds ride through the gated copy untouched — only the enrolled issue
// payload is filtered.
func TestApplyConvergenceKickAdmission_EnforcePreservesNonIssuePayloads(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	actionable.PRs = github.PRResult{Count: 1, Items: []github.PullRequest{{Repo: "projectbluefin/dakota", Number: 900}}}
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeEnforce}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	got := applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)
	if got.PRs.Count != 1 || len(got.PRs.Items) != 1 || got.PRs.Items[0].Number != 900 {
		t.Fatalf("enforce must not touch the PR payload: %+v", got.PRs)
	}
}

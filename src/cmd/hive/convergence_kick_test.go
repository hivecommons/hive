package main

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// ── #4247/#4263 eval-cycle seam: applyConvergenceKickAdmission ─────────────────
//
// The rollout contract under test: with convergence.mode "off" (the DEFAULT)
// the seam is entirely inert — no projection, no log, no mutation; with
// "shadow" the shared evaluator's would-be withholdings are logged and the
// actionable population is still never touched (observed, never enforced).

func kickTestActionable() *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{
			Count: 2,
			Items: []github.Issue{
				{Repo: "projectbluefin/dakota", Number: 601, Title: "dependent work"},
				{Repo: "projectbluefin/dakota", Number: 700, Title: "unrelated ready work"},
			},
		},
	}
}

// kickTestDashboard wires a real dashboard server whose contribute hub reads a
// real on-disk bead store in which dakota#601 depends on an OPEN blocker — the
// exact production observer/evaluator stack, not a stub.
func kickTestDashboard(t *testing.T) *dashboard.Server {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating bead store: %v", err)
	}
	blocker, err := store.Create("blocking work", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("creating blocker: %v", err)
	}
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent: %v", err)
	}
	if err := store.AddDependency(dependent.ID, blocker.ID); err != nil {
		t.Fatalf("adding dependency: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{
		Config:     &config.Config{},
		BeadStores: map[string]*beads.Store{"scanner": store},
	})
	return srv
}

// TestObserveConvergenceKickAdmission_OffModeIsInert pins the default-off
// guarantee: no log line is emitted and the actionable population is untouched.
func TestObserveConvergenceKickAdmission_OffModeIsInert(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	applyConvergenceKickAdmission(&config.Config{}, srv, actionable, nil, logger)

	if buf.Len() != 0 {
		t.Fatalf("mode=off must log nothing, got: %s", buf.String())
	}
	if len(actionable.Issues.Items) != 2 || actionable.Issues.Items[0].Number != 601 {
		t.Fatalf("mode=off must not touch actionable: %+v", actionable.Issues.Items)
	}

	// A nil config is also off (fail-safe) — and must not panic.
	applyConvergenceKickAdmission(nil, srv, actionable, nil, logger)
	if buf.Len() != 0 {
		t.Fatal("nil config must resolve to off and log nothing")
	}
}

// TestObserveConvergenceKickAdmission_ShadowLogsButNeverEnforces: in shadow the
// shared evaluator's withholding is visible in the log — with the stable reason
// — while the actionable issue list handed onward is byte-for-byte unchanged.
func TestObserveConvergenceKickAdmission_ShadowLogsButNeverEnforces(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	srv := kickTestDashboard(t)
	actionable := kickTestActionable()
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	applyConvergenceKickAdmission(cfg, srv, actionable, nil, logger)

	out := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("WITHHELD")) {
		t.Fatalf("shadow mode must log the would-be withholding, got: %s", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("WaitingForDependency")) {
		t.Fatalf("log must carry the evaluator's stable reason, got: %s", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("enforced=false")) {
		t.Fatalf("log must state nothing was enforced, got: %s", out)
	}
	// NEVER enforced: the raw population is intact for SetLastActionable /
	// BuildKickMessages.
	if len(actionable.Issues.Items) != 2 || actionable.Issues.Items[0].Number != 601 {
		t.Fatalf("shadow mode must not touch actionable: %+v", actionable.Issues.Items)
	}
}

// TestObserveConvergenceKickAdmission_ShadowAllAdmitted: when nothing is
// withheld, shadow says so at debug and enforces nothing.
func TestObserveConvergenceKickAdmission_ShadowAllAdmitted(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}})
	actionable := kickTestActionable()
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}

	var buf bytes.Buffer
	captured := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	applyConvergenceKickAdmission(cfg, srv, actionable, nil, captured)
	if bytes.Contains(buf.Bytes(), []byte("WITHHELD")) {
		t.Fatalf("nothing should be withheld with no ledger records: %s", buf.String())
	}
	if len(actionable.Issues.Items) != 2 {
		t.Fatalf("actionable mutated: %+v", actionable.Issues.Items)
	}
}

// TestObserveConvergenceKickAdmission_NilInputsAreSafe: a hive booted without a
// dashboard (or with nothing enumerated) cannot panic here in any mode.
func TestObserveConvergenceKickAdmission_NilInputsAreSafe(t *testing.T) {
	t.Setenv(config.ConvergenceModeEnvVar, "")
	cfg := &config.Config{Convergence: config.ConvergenceConfig{Mode: config.ConvergenceModeShadow}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	applyConvergenceKickAdmission(cfg, nil, kickTestActionable(), nil, logger)
	applyConvergenceKickAdmission(cfg, kickTestDashboard(t), nil, nil, logger)
	applyConvergenceKickAdmission(cfg, kickTestDashboard(t), kickTestActionable(), nil, nil)
}

package mutation

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// TestBoundaryModeFuncOverridesExecutorMode covers the b.Mode dynamic-mode
// selection branch: when set, it re-derives executor.Mode (and, when Stats is
// present, records it) on every call instead of using the mode baked into the
// Executor at construction time.
func TestBoundaryModeFuncOverridesExecutorMode(t *testing.T) {
	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	stats := &effects.Recorder{}
	b := &Boundary{
		Executor: Executor{Ledger: ledger, Journal: journal, Mode: "off", Now: func() time.Time { return time.Unix(100, 0) }},
		Holder:   "test",
		Stats:    stats,
		Mode:     func() string { return "enforce" },
	}
	called := false
	res, err := b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "x"}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{Provenance: "ok"}, nil
	})
	if err != nil || !called || res.Provenance != "ok" {
		t.Fatalf("execute = (%+v, %v) called=%v, want journaled success via Mode() override", res, err, called)
	}
	if snap := stats.Snapshot(); snap.Mode != "enforce" {
		t.Fatalf("stats.Mode = %q, want enforce (Mode() must be recorded via SetMode)", snap.Mode)
	}
}

// TestBoundaryStatsIncDeniedOnOverlap covers Stats.IncDenied on the enforce
// overlap-denial path.
func TestBoundaryStatsIncDeniedOnOverlap(t *testing.T) {
	b := testBoundary(t, "enforce")
	stats := &effects.Recorder{}
	b.Stats = stats
	held, err := b.Executor.Ledger.Acquire(TaskClaim("acme/widget", "acme/widget#issue_comment/7"), "other", time.Hour, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Executor.Ledger.Release(held.Claim.Key(), held.Epoch, time.Unix(101, 0))
	_, err = b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueComment, Target: "7"}, func(context.Context) (effects.Result, error) {
		t.Fatal("effect must not run when denied")
		return effects.Result{}, nil
	})
	if err == nil {
		t.Fatal("expected denial error")
	}
	if snap := stats.Snapshot(); snap.Denied != 1 {
		t.Fatalf("stats.Denied = %d, want 1", snap.Denied)
	}
}

// TestBoundaryStatsIncFencedAndLogsInShadowMode covers the shadow-mode
// "would have denied" branch: Stats.IncFenced is incremented and, when a
// Logger is set, a warning is emitted instead of blocking the effect.
func TestBoundaryStatsIncFencedAndLogsInShadowMode(t *testing.T) {
	b := testBoundary(t, "shadow")
	stats := &effects.Recorder{}
	b.Stats = stats
	var logbuf bytes.Buffer
	b.Logger = slog.New(slog.NewTextHandler(&logbuf, nil))

	held, err := b.Executor.Ledger.Acquire(TaskClaim("acme/widget", "acme/widget#issue_comment/7"), "other", time.Hour, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Executor.Ledger.Release(held.Claim.Key(), held.Epoch, time.Unix(101, 0))

	called := false
	_, err = b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueComment, Target: "7"}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{Provenance: "ok"}, nil
	})
	if err != nil || !called {
		t.Fatalf("shadow mode must not block: err=%v called=%v", err, called)
	}
	if snap := stats.Snapshot(); snap.Fenced != 1 {
		t.Fatalf("stats.Fenced = %d, want 1", snap.Fenced)
	}
	if logbuf.Len() == 0 {
		t.Fatal("expected a warning to be logged for the shadow-mode fenced denial")
	}
}

// TestBoundaryStatsIncJournaledOnSuccess covers Stats.IncJournaled on a clean
// enforce-mode success.
func TestBoundaryStatsIncJournaledOnSuccess(t *testing.T) {
	b := testBoundary(t, "enforce")
	stats := &effects.Recorder{}
	b.Stats = stats
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestCreate, Target: "feature2"}
	_, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{Provenance: "https://github.com/acme/widget/pull/2"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap := stats.Snapshot(); snap.Journaled != 1 {
		t.Fatalf("stats.Journaled = %d, want 1", snap.Journaled)
	}
}

package mutation

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

// These tests pin Execute's error arms (executor.go): the off-mode
// passthrough of an effect error, the time.Now fallback clock, and the two
// double-failure arms where the journal cannot even record an uncertain or
// unacknowledgeable result — both underlying causes must surface together so
// reconciliation is never silently skipped.

// newSplitExecutor builds an executor whose ledger and journal live in
// SEPARATE directories, so one store's persistence can be broken while the
// other keeps working.
func newSplitExecutor(t *testing.T, mode string) (x Executor, c Claim, journalDir string, now time.Time) {
	t.Helper()
	ledgerDir, journalDir := t.TempDir(), t.TempDir()
	l, err := OpenLedger(filepath.Join(ledgerDir, "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	j, err := OpenJournal(filepath.Join(journalDir, "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	now = time.Now()
	return Executor{Ledger: l, Journal: j, Mode: mode, Now: func() time.Time { return now }},
		TaskClaim("acme/widgets", "acme/widgets#7"), journalDir, now
}

// Row: off mode is a passthrough for FAILURE too — the effect's error returns
// unchanged with a zero Operation, and no store is consulted (none exists).
func TestExecutor_OffModeEffectErrorPassesThrough(t *testing.T) {
	boom := errors.New("remote refused")
	op, err := (Executor{Mode: proof.ModeOff}).Execute(testEffect(), 0, "", func() (string, error) {
		return "", boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("off mode must return the effect's own error, got %v", err)
	}
	if op.Status != "" || op.Result != "" || op.LogicalID != "" {
		t.Fatalf("a failed off-mode effect must yield a zero operation, got %+v", op)
	}
}

// Row: a nil Now field selects the real clock — the executor works without a
// test clock injected, using time.Now for both boundary checks.
func TestExecutor_NilNowUsesRealClock(t *testing.T) {
	x, c, _, _ := newSplitExecutor(t, proof.ModeEnforce)
	x.Now = nil
	g, err := x.Ledger.Acquire(c, "alice", ttl, time.Now())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	op, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		return "acme/widgets#101", nil
	})
	if err != nil || op.Status != StatusApplied {
		t.Fatalf("execute with the real clock: %+v %v", op, err)
	}
}

// Row: the effect errs AND the journal cannot record Unknown — the returned
// error must carry the effect's own error (the uncertainty that still needs
// reconciliation), never just the recording failure.
func TestExecutor_UncertainEffectRecordFailureSurfacesEffectError(t *testing.T) {
	x, c, journalDir, now := newSplitExecutor(t, proof.ModeEnforce)
	g, err := x.Ledger.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	boom := errors.New("connection reset mid-request")
	if _, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		// Begin has already persisted; now the journal's disk goes away
		// before the Unknown result can be recorded.
		breakPersistence(t, journalDir)
		return "", boom
	}); !errors.Is(err, boom) {
		t.Fatalf("the effect's own error must survive a failed Unknown record, got %v", err)
	}
}

// Row: ownership changes mid-effect AND the journal cannot record Unknown —
// the returned error must carry the fence (ErrStaleEpoch), so the caller
// knows the effect may exist externally under an epoch it no longer holds.
func TestExecutor_AckFenceRecordFailureSurfacesFence(t *testing.T) {
	x, c, journalDir, now := newSplitExecutor(t, proof.ModeEnforce)
	g1, err := x.Ledger.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := x.Execute(testEffect(), g1.Epoch, "alice", func() (string, error) {
		// Mid-effect: the hold is fenced and reassigned (the ledger's disk
		// stays healthy), then the journal's disk goes away.
		if _, err := x.Ledger.Wait(c.Key(), g1.Epoch, now); err != nil {
			return "", fmt.Errorf("mid-flight wait: %w", err)
		}
		if _, err := x.Ledger.Acquire(c, "bob", ttl, now); err != nil {
			return "", fmt.Errorf("mid-flight reacquire: %w", err)
		}
		breakPersistence(t, journalDir)
		return "acme/widgets#101", nil // the external effect DID happen
	}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("the fence must survive a failed Unknown record, got %v", err)
	}
}

package mutation

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

func newExecutor(t *testing.T, mode string) (Executor, Claim, time.Time) {
	t.Helper()
	dir := t.TempDir()
	l, err := OpenLedger(filepath.Join(dir, "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	j, err := OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	now := time.Now()
	return Executor{Ledger: l, Journal: j, Mode: mode, Now: func() time.Time { return now }},
		TaskClaim("acme/widgets", "acme/widgets#7"), now
}

// Row: the current epoch holder executes the selected effect — it occurs once
// with durable result/provenance; a disjoint claim/effect proceeds as the
// positive control.
func TestExecutor_CurrentEpochExecutesOnce(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g, err := x.Ledger.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	calls := 0
	op, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		calls++
		return "acme/widgets#101", nil
	})
	if err != nil || op.Status != StatusApplied || op.Result != "acme/widgets#101" || calls != 1 {
		t.Fatalf("execute: %+v calls=%d err=%v", op, calls, err)
	}
	// The exact same desired effect can never run twice.
	if _, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		calls++
		return "dup", nil
	}); !errors.Is(err, ErrAlreadyApplied) || calls != 1 {
		t.Fatalf("duplicate execute must be refused before the effect runs: %v calls=%d", err, calls)
	}

	// Positive control: a disjoint claim and effect proceed.
	c9 := TaskClaim("acme/gadgets", "acme/gadgets#1")
	g9, err := x.Ledger.Acquire(c9, "bob", ttl, now)
	if err != nil {
		t.Fatalf("disjoint acquire: %v", err)
	}
	e9 := testEffect()
	e9.Subject = "acme/gadgets#1"
	e9.ClaimKey = c9.Key()
	if op, err := x.Execute(e9, g9.Epoch, "bob", func() (string, error) { return "acme/gadgets#5", nil }); err != nil || op.Status != StatusApplied {
		t.Fatalf("disjoint effect must proceed: %+v %v", op, err)
	}
}

// Row: stale-owner mutation and acknowledgment are fenced at the real
// boundary — before the effect runs at all.
func TestExecutor_StaleEpochFencedBeforeEffect(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g1, _ := x.Ledger.Acquire(c, "alice", ttl, now)
	if _, err := x.Ledger.Wait(c.Key(), g1.Epoch, now); err != nil {
		t.Fatalf("wait: %v", err)
	}
	g2, err := x.Ledger.Acquire(c, "bob", ttl, now)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}

	calls := 0
	if _, err := x.Execute(testEffect(), g1.Epoch, "alice", func() (string, error) {
		calls++
		return "", nil
	}); !errors.Is(err, ErrStaleEpoch) || calls != 0 {
		t.Fatalf("stale epoch must be fenced BEFORE the effect: %v calls=%d", err, calls)
	}
	// The current owner proceeds.
	if op, err := x.Execute(testEffect(), g2.Epoch, "bob", func() (string, error) { return "acme/widgets#101", nil }); err != nil || op.Status != StatusApplied {
		t.Fatalf("current epoch must proceed: %+v %v", op, err)
	}
}

// Row: ownership changes while the effect is in flight — the stale owner may
// not ACKNOWLEDGE; the result is recorded Unknown and the new owner
// reconciles the SAME logical operation from authoritative external state.
func TestExecutor_AckFencedMidFlight_NewOwnerAdopts(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g1, _ := x.Ledger.Acquire(c, "alice", ttl, now)

	e := testEffect()
	if _, err := x.Execute(e, g1.Epoch, "alice", func() (string, error) {
		// Mid-effect: the hold enters a wait (revocation path) and bob takes
		// over — g1 is fenced by the time acknowledgment is attempted.
		if _, err := x.Ledger.Wait(c.Key(), g1.Epoch, now); err != nil {
			t.Fatalf("mid-flight wait: %v", err)
		}
		if _, err := x.Ledger.Acquire(c, "bob", ttl, now); err != nil {
			t.Fatalf("mid-flight reacquire: %v", err)
		}
		return "acme/widgets#101", nil // the external effect DID happen
	}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale owner must not acknowledge: %v", err)
	}

	op, ok := x.Journal.Get(e.LogicalID())
	if !ok || op.Status != StatusUnknown {
		t.Fatalf("unacknowledgeable effect must be Unknown pending reconciliation: %+v", op)
	}
	// The new owner adopts and reconciles the SAME logical operation: the
	// authoritative query finds the created PR — Applied, no duplicate.
	rec, err := x.Journal.Reconcile(e.LogicalID(), ExternalState{Known: true, Applied: true, Result: "acme/widgets#101"}, now)
	if err != nil || rec.Status != StatusApplied {
		t.Fatalf("adoption reconciliation: %+v %v", rec, err)
	}
	if _, err := x.Journal.Begin(e, 2, "bob", now); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("unchanged inputs can never become a second operation: %v", err)
	}
}

// Row: an uncertain effect (error) records Unknown; retry without
// reconciliation is refused; reconciliation to NotApplied authorizes exactly
// one retry under the current epoch.
func TestExecutor_UncertainEffectReconcilesBeforeRetry(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g, _ := x.Ledger.Acquire(c, "alice", ttl, now)
	e := testEffect()

	if _, err := x.Execute(e, g.Epoch, "alice", func() (string, error) {
		return "", fmt.Errorf("connection reset mid-request")
	}); err == nil {
		t.Fatal("uncertain effect must surface an error")
	}
	// Blind retry refused.
	if _, err := x.Execute(e, g.Epoch, "alice", func() (string, error) { return "dup", nil }); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("retry before reconciliation must be refused: %v", err)
	}
	// Authoritative state: not applied → one retry proceeds.
	if _, err := x.Journal.Reconcile(e.LogicalID(), ExternalState{Known: true, Applied: false}, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	op, err := x.Execute(e, g.Epoch, "alice", func() (string, error) { return "acme/widgets#101", nil })
	if err != nil || op.Status != StatusApplied || len(op.Attempts) != 2 {
		t.Fatalf("reconciled retry must adopt the same operation: %+v %v", op, err)
	}
}

// Row: mode inertness. Off touches neither ledger nor journal and runs the
// effect directly (byte-identical legacy behavior); shadow records but never
// fences — even a stale epoch executes.
func TestExecutor_ModeGating(t *testing.T) {
	// Off: no ledger/journal needed at all.
	off := Executor{Mode: proof.ModeOff}
	calls := 0
	op, err := off.Execute(testEffect(), 0, "", func() (string, error) {
		calls++
		return "legacy", nil
	})
	if err != nil || op.Result != "legacy" || calls != 1 {
		t.Fatalf("off mode must be a passthrough: %+v %v", op, err)
	}
	for _, mode := range []string{"", "ENFORCE", "on", "bogus"} {
		if _, err := (Executor{Mode: mode}).Execute(testEffect(), 0, "", func() (string, error) { return "x", nil }); err != nil {
			t.Fatalf("unrecognised mode %q must fail safe to off: %v", mode, err)
		}
	}

	// Shadow: records, never withholds — a stale epoch still executes.
	x, c, now := newExecutor(t, proof.ModeShadow)
	g1, _ := x.Ledger.Acquire(c, "alice", ttl, now)
	if _, err := x.Ledger.Wait(c.Key(), g1.Epoch, now); err != nil {
		t.Fatalf("wait: %v", err)
	}
	op, err = x.Execute(testEffect(), g1.Epoch, "alice", func() (string, error) { return "shadow-pr", nil })
	if err != nil || op.Status != StatusApplied {
		t.Fatalf("shadow must never withhold: %+v %v", op, err)
	}
	if _, ok := x.Journal.Get(testEffect().LogicalID()); !ok {
		t.Fatal("shadow must still record the operation")
	}

	// Enabled modes require the durable stores.
	if _, err := (Executor{Mode: proof.ModeEnforce}).Execute(testEffect(), 1, "a", func() (string, error) { return "", nil }); err == nil {
		t.Fatal("enforce without stores must refuse")
	}
}

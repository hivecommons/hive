package mutation

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// These tests cover the cross-process serialization property required by the
// step-3 handoff evaluation of RFC #4002
// (src/docs/design/agent-turn-handoff.md). Two handles opened before either
// writes must still observe a single durable owner and must merge independent
// claims instead of overwriting a peer's file snapshot.

// TestTwoOpenLedgersBothAcquireTheSameClaim verifies the ledger's durable CAS
// holds across independent handles, not just goroutines sharing one mutex.
func TestTwoOpenLedgersBothAcquireTheSameClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	claim := TaskClaim("hivecommons/hive", "hivecommons/hive#4002")
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	// Both handles are opened BEFORE either acquires — the handoff shape, where
	// the replacement is already running when the incumbent still holds.
	first, err := OpenLedger(path, DefaultMaxWritersPerRepo)
	if err != nil {
		t.Fatalf("opening first ledger: %v", err)
	}
	second, err := OpenLedger(path, DefaultMaxWritersPerRepo)
	if err != nil {
		t.Fatalf("opening second ledger: %v", err)
	}

	a, err := first.Acquire(claim, "spoke-a", time.Hour, now)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err = second.Acquire(claim, "spoke-b", time.Hour, now)
	if err != nil {
		if !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("second acquire: %v", err)
		}
		return
	}
	t.Fatalf("second acquire unexpectedly succeeded after first holder %s epoch %d", a.Holder, a.Epoch)
}

// TestReopeningAfterAConcurrentAcquireSeesOnlyTheLastWriter verifies two
// handles that acquire disjoint claims under available capacity preserve both
// records in the durable ledger.
func TestReopeningAfterAConcurrentAcquireSeesOnlyTheLastWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	other := TaskClaim("hivecommons/hive", "hivecommons/hive#4000")
	claim := TaskClaim("hivecommons/hive", "hivecommons/hive#4002")
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	first, err := OpenLedger(path, 8)
	if err != nil {
		t.Fatalf("opening first ledger: %v", err)
	}
	second, err := OpenLedger(path, 8)
	if err != nil {
		t.Fatalf("opening second ledger: %v", err)
	}

	// Distinct, non-overlapping claims must both survive the second handle's
	// refresh-and-rewrite cycle.
	if _, err := first.Acquire(other, "spoke-a", time.Hour, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := second.Acquire(claim, "spoke-b", time.Hour, now); err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	recovered, err := OpenLedger(path, 8)
	if err != nil {
		t.Fatalf("reopening ledger: %v", err)
	}
	if _, ok := recovered.Get(claim.Key()); !ok {
		t.Fatalf("last writer's claim %s is missing from the reloaded ledger", claim.Key())
	}
	if _, ok := recovered.Get(other.Key()); !ok {
		t.Fatalf("first writer's claim %s was lost during the second handle's rewrite", other.Key())
	}
}

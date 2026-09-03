package mutation

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// These are CHARACTERIZATION tests for the step-3 handoff evaluation of RFC
// #4002 (src/docs/design/agent-turn-handoff.md). The Ledger's CAS is documented
// as serialized by "the ledger mutex" (ledger.go:81) — a sync.Mutex, which is
// per-process. The package is inert today (nothing outside it calls OpenLedger),
// so this is not a live defect; it is the property a cross-process handoff would
// have to close before reusing this ledger as its lease.
//
// Nothing here asserts the current behaviour is desirable. If a later change
// adds cross-process serialization these tests skip with a rewrite instruction
// rather than failing opaquely.

// TestTwoOpenLedgersBothAcquireTheSameClaim pins the gap. Each handle keeps its
// own in-memory index and its own mutex, so the overlap scan in Acquire consults
// only what THAT handle has seen. Two processes holding the ledger open across
// an acquisition therefore both win — at the same epoch, which is what makes it
// a fencing failure rather than merely a duplicate grant.
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
	b, err := second.Acquire(claim, "spoke-b", time.Hour, now)
	if err != nil {
		if errors.Is(err, ErrClaimHeld) {
			t.Skipf("second acquire was refused (%v) — the ledger has gained "+
				"cross-process serialization; rewrite this test as that guarantee", err)
		}
		t.Fatalf("second acquire: %v", err)
	}

	if a.Epoch != b.Epoch {
		t.Fatalf("epochs %d and %d differ — the second handle saw the first's "+
			"entry, so the step-3 evaluation's premise needs revisiting", a.Epoch, b.Epoch)
	}
	// Two holders at one epoch: ValidateEpoch cannot tell them apart, so the
	// fence authorizes both.
	if err := first.ValidateEpoch(claim.Key(), a.Epoch, now); err != nil {
		t.Fatalf("first holder fenced out at its own epoch: %v", err)
	}
	if err := second.ValidateEpoch(claim.Key(), b.Epoch, now); err != nil {
		t.Fatalf("second holder fenced out at its own epoch: %v", err)
	}
}

// TestReopeningAfterAConcurrentAcquireSeesOnlyTheLastWriter pins the durable
// consequence: persistLocked rewrites the whole file from one handle's index,
// so the loser's record is not merged, it is erased. A third process booting
// afterwards — the actual restart-recovery path — reads a ledger that never
// mentions the first holder.
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

	// Distinct, non-overlapping claims: even with no contention at all, the
	// two handles cannot both survive a rewrite.
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
	if _, ok := recovered.Get(other.Key()); ok {
		t.Skipf("both claims survived — the ledger now merges concurrent writers; " +
			"rewrite this test as that guarantee")
	}
}

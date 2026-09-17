package mutation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the ledger's durable-before-authoritative invariant
// (ledger.go Acquire/transition): when the durable write fails, the in-memory
// index MUST roll back so no epoch or state exists in memory that a restart
// would forget. All three rollback arms are exercised: deletion of a
// brand-new entry, restoration of the replaced prior entry, and restoration
// of the pre-transition state.

// Row: Acquire's persist fails for a NEW key — the entry must vanish from
// memory (no phantom epoch was ever granted), no ledger file may appear, and
// once the disk is healthy the same claim acquires at epoch 1, proving the
// failed commit minted nothing.
func TestLedger_CommitRollback_NewEntryDeletedOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(filepath.Join(dir, "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	now := time.Now()

	restore := breakPersistence(t, dir)

	if _, err := l.Acquire(c, "alice", ttl, now); err == nil {
		t.Fatal("Acquire must fail when the durable write fails")
	}
	if _, ok := l.Get(c.Key()); ok {
		t.Fatal("failed Acquire must not leave a phantom entry in memory")
	}
	if _, err := os.Stat(filepath.Join(dir, "claims.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no ledger file may exist after a failed first persist: %v", err)
	}

	restore()
	g, err := l.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("Acquire after recovery: %v", err)
	}
	if g.Epoch != 1 {
		t.Fatalf("failed commit must mint no epoch: recovered acquisition got epoch %d, want 1", g.Epoch)
	}
}

// Row: a reacquisition's persist fails — the PRIOR entry must be restored
// exactly (its epoch and state), the disk must still hold the prior state,
// and once the disk is healthy the reacquisition mints strictly epoch+1 as if
// the failed attempt never happened.
func TestLedger_CommitRollback_PriorEntryRestoredOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claims.json")
	l, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	now := time.Now()
	g1, err := l.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := l.Wait(c.Key(), g1.Epoch, now); err != nil {
		t.Fatalf("wait: %v", err)
	}

	restore := breakPersistence(t, dir)

	if _, err := l.Acquire(c, "bob", ttl, now); err == nil {
		t.Fatal("reacquisition must fail when the durable write fails")
	}
	e, ok := l.Get(c.Key())
	if !ok || e.Epoch != g1.Epoch || e.State != StateWaiting || e.Holder != "alice" {
		t.Fatalf("failed reacquisition must restore the prior entry exactly, got %+v", e)
	}
	// Disk (still readable) agrees: memory never outran the file.
	reopened, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("reopening ledger mid-outage: %v", err)
	}
	if d, ok := reopened.Get(c.Key()); !ok || d.Epoch != g1.Epoch || d.State != StateWaiting {
		t.Fatalf("disk must still hold the prior entry, got %+v", d)
	}

	restore()
	g2, err := l.Acquire(c, "bob", ttl, now)
	if err != nil {
		t.Fatalf("reacquire after recovery: %v", err)
	}
	if g2.Epoch != g1.Epoch+1 {
		t.Fatalf("recovered reacquisition must mint epoch %d, got %d — the failed attempt may not consume epochs", g1.Epoch+1, g2.Epoch)
	}
}

// Row: a state transition's persist fails — the entry must remain in its
// prior state at its prior epoch, still authorized (ValidateEpoch passes),
// and the same transition lands once the disk is healthy. A hold can never be
// half-released.
func TestLedger_CommitRollback_TransitionRestoresStateOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(filepath.Join(dir, "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	now := time.Now()
	g, err := l.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	restore := breakPersistence(t, dir)

	if _, err := l.Wait(c.Key(), g.Epoch, now); err == nil {
		t.Fatal("Wait must fail when the durable write fails")
	}
	e, ok := l.Get(c.Key())
	if !ok || e.State != StateActiveMutation || e.Epoch != g.Epoch {
		t.Fatalf("failed transition must restore the prior state, got %+v", e)
	}
	if err := l.ValidateEpoch(c.Key(), g.Epoch, now); err != nil {
		t.Fatalf("the hold must still authorize after a failed transition: %v", err)
	}

	restore()
	w, err := l.Wait(c.Key(), g.Epoch, now)
	if err != nil {
		t.Fatalf("Wait after recovery: %v", err)
	}
	if w.State != StateWaiting || w.Epoch != g.Epoch {
		t.Fatalf("recovered Wait must land the transition at the same epoch, got %+v", w)
	}
}

// Row: OpenLedger's refusal arms beyond unparseable JSON (already pinned by
// TestLedger_CorruptFileRefuses): an unreadable file, an entry whose claim
// fails Validate, and two entries for the same key each refuse to open, and
// each leaves the bytes exactly as found for inspection.
func TestOpenLedger_UnreadableInvalidAndConflictingEntriesRefuse(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("unreadable file does not block root")
	}

	unreadable := filepath.Join(t.TempDir(), "claims.json")
	if err := writeFile(unreadable, `{"version":1,"entries":[]}`); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := OpenLedger(unreadable, 0); err == nil {
		t.Fatal("an unreadable ledger must refuse to open")
	}

	// A task claim without a subject fails Validate.
	invalid := filepath.Join(t.TempDir(), "claims.json")
	invalidBytes := `{"version":1,"entries":[{"claim":{"type":"task","repo":"acme/widgets"},"holder":"alice","epoch":1,"state":"ActiveMutation"}]}`
	if err := writeFile(invalid, invalidBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(invalid, 0); err == nil {
		t.Fatal("a ledger holding an invalid claim must refuse to open")
	}
	if got := readFile(t, invalid); got != invalidBytes {
		t.Fatalf("refused bytes must be left for inspection, got %q", got)
	}

	entry := `{"claim":{"type":"task","repo":"acme/widgets","subject":"acme/widgets#7"},"holder":"alice","epoch":1,"state":"ActiveMutation"}`
	dup := filepath.Join(t.TempDir(), "claims.json")
	dupBytes := `{"version":1,"entries":[` + entry + `,` + entry + `]}`
	if err := writeFile(dup, dupBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(dup, 0); err == nil {
		t.Fatal("a ledger holding conflicting entries for one key must refuse to open")
	}
	if got := readFile(t, dup); got != dupBytes {
		t.Fatalf("refused bytes must be left for inspection, got %q", got)
	}
}

package mutation

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These tests cover the cross-process serialization property the operation
// journal must share with the claim ledger (#8288): two handles opened on one
// path before either writes must merge independent operations instead of
// persisting an older whole-file snapshot over a peer's transition, and a
// lock that cannot be taken within the bounded wait must surface as an error.

func crossProcessEffect(subject, head string) Effect {
	e := testEffect()
	e.Subject = subject
	e.ClaimKey = TaskClaim("acme/widgets", subject).Key()
	e.Inputs = map[string]string{"repo": "acme/widgets", "head": head, "base": "main"}
	return e
}

// TestTwoOpenJournalsInterleavedCommitsLoseNothing verifies two handles that
// Begin and RecordResult different logical IDs in an interleaved order both
// have every operation present after a fresh reload.
func TestTwoOpenJournalsInterleavedCommitsLoseNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	ea := crossProcessEffect("acme/widgets#1", "feature-a")
	eb := crossProcessEffect("acme/widgets#2", "feature-b")

	// Both handles are opened BEFORE either writes, so each starts from the
	// same empty snapshot and would overwrite the other without the flock.
	first, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("opening first journal: %v", err)
	}
	second, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("opening second journal: %v", err)
	}

	if _, err := first.Begin(ea, 1, "spoke-a", now); err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if _, err := second.Begin(eb, 1, "spoke-b", now); err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if _, err := first.RecordResult(ea.LogicalID(), 1, StatusApplied, "https://github.com/acme/widgets/pull/11", now); err != nil {
		t.Fatalf("first RecordResult: %v", err)
	}
	if _, err := second.RecordResult(eb.LogicalID(), 1, StatusNotApplied, "", now); err != nil {
		t.Fatalf("second RecordResult: %v", err)
	}

	recovered, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopening journal: %v", err)
	}
	opA, ok := recovered.Get(ea.LogicalID())
	if !ok {
		t.Fatalf("first handle's operation %s was lost during the second handle's rewrite", ea.LogicalID())
	}
	if opA.Status != StatusApplied || opA.Result != "https://github.com/acme/widgets/pull/11" {
		t.Fatalf("first handle's result was lost: %+v", opA)
	}
	opB, ok := recovered.Get(eb.LogicalID())
	if !ok {
		t.Fatalf("second handle's operation %s is missing from the reloaded journal", eb.LogicalID())
	}
	if opB.Status != StatusNotApplied {
		t.Fatalf("second handle's result was lost: %+v", opB)
	}

	// Each handle also observes the peer's transition through its own Get,
	// because every read refreshes from the locked on-disk snapshot.
	if op, ok := first.Get(eb.LogicalID()); !ok || op.Status != StatusNotApplied {
		t.Fatalf("first handle must observe the second handle's operation, got ok=%v op=%+v", ok, op)
	}
	if op, ok := second.Get(ea.LogicalID()); !ok || op.Status != StatusApplied {
		t.Fatalf("second handle must observe the first handle's operation, got ok=%v op=%+v", ok, op)
	}
}

// TestJournalLockFileCreatedLazilyBesidePath verifies opening never touches
// the directory (an absent data dir must not refuse boot) and that the flock
// sibling appears beside the journal path on the first transition.
func TestJournalLockFileCreatedLazilyBesidePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal on an absent directory must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opening must not create the journal directory: %v", err)
	}
	if _, err := j.Begin(crossProcessEffect("acme/widgets#6", "feature-f"), 1, "spoke-a", time.Now()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := os.Stat(path + journalLockFileSuffix); err != nil {
		t.Fatalf("lock file must exist beside the journal path after the first transition: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal file must exist after the first transition: %v", err)
	}
}

// TestReconcileAcrossHandlesSeesPeerTransition verifies Reconcile goes
// through the same lock-and-refresh discipline: a handle that never saw the
// peer's Begin still reconciles the peer's operation, and the peer then
// observes the reconciled status.
func TestReconcileAcrossHandlesSeesPeerTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	e := crossProcessEffect("acme/widgets#3", "feature-c")
	id := e.LogicalID()

	first, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("opening first journal: %v", err)
	}
	second, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("opening second journal: %v", err)
	}

	if _, err := first.Begin(e, 1, "spoke-a", now); err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	// The second handle was opened on an empty snapshot; without refreshing
	// under the lock it would report ErrUnknownOperation.
	op, err := second.Reconcile(id, ExternalState{Known: true, Applied: true, Result: "https://github.com/acme/widgets/pull/12"}, now)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if op.Status != StatusApplied {
		t.Fatalf("reconcile must resolve the peer's Planned entry to Applied, got %+v", op)
	}
	// The first handle sees the reconciled terminal truth: a retry refuses.
	if _, err := first.Begin(e, 2, "spoke-a", now); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("first handle must observe the reconciled Applied entry, got %v", err)
	}
	if got, ok := first.Get(id); !ok || got.Status != StatusApplied || got.Result != "https://github.com/acme/widgets/pull/12" {
		t.Fatalf("first handle must read the reconciled result, got ok=%v op=%+v", ok, got)
	}
}

// TestJournalLockTimeoutSurfacesAsError verifies a lock held by another
// process for longer than the bounded wait yields ErrJournalLocked from
// Begin, RecordResult, and Reconcile, and that the refused transition applied
// nothing.
func TestJournalLockTimeoutSurfacesAsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	e := crossProcessEffect("acme/widgets#4", "feature-d")
	id := e.LogicalID()

	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if _, err := j.Begin(e, 1, "spoke-a", now); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	j.lockTimeout = 50 * time.Millisecond

	// Hold the flock from an independent open file description, exactly as a
	// peer process would.
	holder, err := os.OpenFile(path+journalLockFileSuffix, os.O_RDWR, journalFileMode)
	if err != nil {
		t.Fatalf("opening lock file: %v", err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("holding lock: %v", err)
	}

	if _, err := j.Begin(crossProcessEffect("acme/widgets#5", "feature-e"), 1, "spoke-b", now); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("Begin under a held lock must time out with ErrJournalLocked, got %v", err)
	}
	if _, err := j.RecordResult(id, 1, StatusApplied, "https://github.com/acme/widgets/pull/13", now); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("RecordResult under a held lock must time out with ErrJournalLocked, got %v", err)
	}
	if _, err := j.Reconcile(id, ExternalState{Known: true, Applied: true}, now); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("Reconcile under a held lock must time out with ErrJournalLocked, got %v", err)
	}

	// Releasing the peer's lock lets the same transitions proceed, and the
	// refused attempts applied nothing in the meantime.
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("releasing lock: %v", err)
	}
	op, err := j.RecordResult(id, 1, StatusApplied, "https://github.com/acme/widgets/pull/13", now)
	if err != nil {
		t.Fatalf("RecordResult after release: %v", err)
	}
	if op.Status != StatusApplied || op.Attempts[0].Outcome != "applied" {
		t.Fatalf("result must land once the lock is free: %+v", op)
	}
	reloaded, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reloaded.Get(crossProcessEffect("acme/widgets#5", "feature-e").LogicalID()); ok {
		t.Fatal("a timed-out Begin must not have persisted its intent")
	}
}

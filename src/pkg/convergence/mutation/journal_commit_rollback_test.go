package mutation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the commit() crash-safety invariant (journal.go): when the
// durable write fails, the in-memory index MUST roll back so process memory
// never claims state the disk cannot prove. Both rollback arms are exercised:
// the delete of a brand-new entry and the restore of the prior entry.

// breakPersistence makes the journal directory unwritable so persistLocked's
// tmp-file write fails; restore undoes it via t.Cleanup and the returned func.
func breakPersistence(t *testing.T, dir string) (restore func()) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("read-only dir does not block root")
	}
	if err := os.Chmod(dir, 0o550); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	restore = func() {
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatalf("restore chmod %s: %v", dir, err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// Row: Begin's persist fails for a NEW logical ID — the entry must vanish
// from memory (no phantom intent), no journal file may appear, and once the
// disk is healthy again the same Begin succeeds as attempt #1.
func TestJournal_CommitRollback_NewEntryDeletedOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	e := testEffect()
	id := e.LogicalID()
	now := time.Now()

	restore := breakPersistence(t, dir)

	if _, err := j.Begin(e, 1, "alice", now); err == nil {
		t.Fatal("Begin must fail when the durable write fails")
	}
	if _, ok := j.Get(id); ok {
		t.Fatal("failed Begin must not leave a phantom entry in memory")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no journal file may exist after a failed first persist: %v", err)
	}

	// Disk healthy again: the identical effect begins cleanly as attempt #1,
	// proving the failed commit authorized nothing.
	restore()
	op, err := j.Begin(e, 1, "alice", now)
	if err != nil {
		t.Fatalf("Begin after recovery: %v", err)
	}
	if op.Status != StatusPlanned || len(op.Attempts) != 1 {
		t.Fatalf("recovered Begin must record attempt #1 Planned: %+v", op)
	}
}

// Row: RecordResult's persist fails for an EXISTING entry — memory must
// restore the prior operation exactly (status, attempts, outcome), the disk
// must still hold the prior state, and the same result lands after recovery.
func TestJournal_CommitRollback_PriorEntryRestoredOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	e := testEffect()
	id := e.LogicalID()
	now := time.Now()

	if _, err := j.Begin(e, 1, "alice", now); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	restore := breakPersistence(t, dir)

	if _, err := j.RecordResult(id, 1, StatusApplied, "https://github.com/acme/widgets/pull/9", now); err == nil {
		t.Fatal("RecordResult must fail when the durable write fails")
	}
	got, ok := j.Get(id)
	if !ok {
		t.Fatal("prior entry must survive a failed commit")
	}
	if got.Status != StatusPlanned || got.Result != "" {
		t.Fatalf("memory must roll back to the prior Planned entry, got %+v", got)
	}
	if n := len(got.Attempts); n != 1 || got.Attempts[0].Outcome != "" {
		t.Fatalf("attempt outcome must roll back to unresolved, got %+v", got.Attempts)
	}

	// Disk still proves only the prior state: a fresh open sees Planned.
	reloaded, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen after failed commit: %v", err)
	}
	if op, ok := reloaded.Get(id); !ok || op.Status != StatusPlanned {
		t.Fatalf("disk must still hold the prior Planned entry, got ok=%v op=%+v", ok, op)
	}

	// Recovery: the identical result now commits and becomes terminal truth.
	restore()
	op, err := j.RecordResult(id, 1, StatusApplied, "https://github.com/acme/widgets/pull/9", now)
	if err != nil {
		t.Fatalf("RecordResult after recovery: %v", err)
	}
	if op.Status != StatusApplied || op.Attempts[0].Outcome != "applied" {
		t.Fatalf("recovered result must be Applied with outcome recorded: %+v", op)
	}
}

// Row: Reconcile's persist fails — reconciliation must not update memory it
// cannot prove on disk; the unresolved entry keeps demanding reconciliation,
// and after recovery the same reconciliation resolves it.
func TestJournal_CommitRollback_ReconcilePersistFailureKeepsUnresolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	e := testEffect()
	id := e.LogicalID()
	now := time.Now()

	if _, err := j.Begin(e, 1, "alice", now); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	restore := breakPersistence(t, dir)

	if _, err := j.Reconcile(id, ExternalState{Known: true, Applied: false}, now); err == nil {
		t.Fatal("Reconcile must fail when the durable write fails")
	}
	// The failed reconciliation authorized nothing: retry still refuses.
	if _, err := j.Begin(e, 2, "bob", now); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("entry must remain unresolved after failed reconcile commit: %v", err)
	}

	restore()
	if _, err := j.Reconcile(id, ExternalState{Known: true, Applied: false}, now); err != nil {
		t.Fatalf("Reconcile after recovery: %v", err)
	}
	op, err := j.Begin(e, 2, "bob", now)
	if err != nil {
		t.Fatalf("Begin after recovered reconcile: %v", err)
	}
	if op.Status != StatusPlanned || len(op.Attempts) != 2 {
		t.Fatalf("recovered reconcile must authorize exactly one retry: %+v", op)
	}
}

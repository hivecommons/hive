package mutation

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// effectN derives a distinct logical operation per generation so a test can
// populate the journal with many independent entries.
func effectN(n int) Effect {
	e := testEffect()
	e.DesiredGeneration = n
	e.Inputs = map[string]string{"repo": "acme/widgets", "head": fmt.Sprintf("feature-%d", n), "base": "main"}
	return e
}

// applyN journals one Applied operation for generation n at instant at.
func applyN(t *testing.T, j *Journal, n int, at time.Time) Operation {
	t.Helper()
	op, err := j.Begin(effectN(n), 1, "alice", at)
	if err != nil {
		t.Fatalf("Begin %d: %v", n, err)
	}
	op, err = j.RecordResult(op.LogicalID, 1, StatusApplied, fmt.Sprintf("pr-%d", n), at)
	if err != nil {
		t.Fatalf("RecordResult %d: %v", n, err)
	}
	return op
}

func countOps(j *Journal) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.ops)
}

// A terminal entry older than the retention window is dropped by the next
// commit, both in memory and from the persisted snapshot; a fresh terminal
// entry and an unresolved entry survive.
func TestJournal_PrunesTerminalEntriesPastRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	j.retention = time.Hour
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	stale := applyN(t, j, 1, base)
	planned, err := j.Begin(effectN(2), 1, "alice", base)
	if err != nil {
		t.Fatalf("Begin planned: %v", err)
	}
	fresh := applyN(t, j, 3, base.Add(2*time.Hour))

	if _, ok := j.Get(stale.LogicalID); ok {
		t.Fatal("terminal entry past retention must be pruned")
	}
	if _, ok := j.Get(planned.LogicalID); !ok {
		t.Fatal("unresolved (Planned) entry must never be pruned regardless of age")
	}
	if _, ok := j.Get(fresh.LogicalID); !ok {
		t.Fatal("terminal entry inside the retention window must be kept")
	}

	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := countOps(reopened); got != 2 {
		t.Fatalf("persisted snapshot must reflect the prune: %d entries, want 2", got)
	}
}

// The entry count never exceeds maxEntries: the oldest terminal entries are
// evicted first while Planned/Unknown entries are always retained.
func TestJournal_EvictsOldestTerminalEntriesPastMaxEntries(t *testing.T) {
	j := openJournal(t)
	j.retention = 0
	j.maxEntries = 5
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	unknown, err := j.Begin(effectN(100), 1, "alice", base)
	if err != nil {
		t.Fatalf("Begin unknown: %v", err)
	}
	if _, err := j.Reconcile(unknown.LogicalID, ExternalState{Known: false}, base); err != nil {
		t.Fatalf("Reconcile unknown: %v", err)
	}

	var ids []string
	for n := 1; n <= 12; n++ {
		op := applyN(t, j, n, base.Add(time.Duration(n)*time.Minute))
		ids = append(ids, op.LogicalID)
		if got := countOps(j); got > j.maxEntries {
			t.Fatalf("after %d commits journal holds %d entries, bound is %d", n, got, j.maxEntries)
		}
	}
	if got := countOps(j); got != j.maxEntries {
		t.Fatalf("journal should sit exactly at the bound: %d != %d", got, j.maxEntries)
	}
	if _, ok := j.Get(unknown.LogicalID); !ok {
		t.Fatal("Unknown entry must survive eviction")
	}
	// Four newest terminal entries remain alongside the Unknown one.
	for i, id := range ids {
		_, ok := j.Get(id)
		if wantKept := i >= len(ids)-(j.maxEntries-1); ok != wantKept {
			t.Fatalf("entry %d kept=%v want %v", i+1, ok, wantKept)
		}
	}
}

// With bounds disabled (zero values) nothing is ever pruned.
func TestJournal_ZeroBoundsDisablePruning(t *testing.T) {
	j := openJournal(t)
	j.retention = 0
	j.maxEntries = 0
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for n := 1; n <= 5; n++ {
		applyN(t, j, n, base.Add(time.Duration(n)*365*24*time.Hour))
	}
	if got := countOps(j); got != 5 {
		t.Fatalf("unbounded journal must keep every entry: %d", got)
	}
}

// Retention is measured against the journal's own clock: a commit carrying a
// forged future timestamp must not age out other terminal entries.
func TestJournal_PruneIgnoresForgedFutureTimestamp(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	j.retention = time.Hour
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	j.now = func() time.Time { return base }

	kept := applyN(t, j, 1, base.Add(-30*time.Minute))
	applyN(t, j, 2, base.Add(100*time.Hour))

	if _, ok := j.Get(kept.LogicalID); !ok {
		t.Fatal("a forged future timestamp must not prune entries inside the retention window")
	}
}

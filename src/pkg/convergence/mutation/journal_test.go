package mutation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o660)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func openJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	return j
}

func TestDeriveLogicalIDExcludesAttemptMetadata(t *testing.T) {
	parts := []string{"v1", "session", "comment", "kubestellar/hive", "5624", "same body"}
	first := DeriveLogicalID(parts, nil)
	second := DeriveLogicalID(append([]string(nil), parts...), nil)
	if first == "" || first != second {
		t.Fatalf("DeriveLogicalID must be stable: first=%q second=%q", first, second)
	}
	if changed := DeriveLogicalID([]string{"v1", "session", "comment", "kubestellar/hive", "5624", "different body"}, nil); changed == first {
		t.Fatal("changing a load-bearing field must change the logical operation id")
	}
}

// Row: crash after intent, before the effect — replay finds the same logical
// operation; reconciliation to NotApplied authorizes at most one retry.
func TestJournal_CrashBeforeEffect_ReconcilesThenRetries(t *testing.T) {
	j := openJournal(t)
	e := testEffect()
	now := time.Now()

	op, err := j.Begin(e, 1, "alice", now)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if op.Status != StatusPlanned || len(op.Attempts) != 1 {
		t.Fatalf("intent must be durable before the effect: %+v", op)
	}
	// Replay (same or new owner) finds the SAME entry and may not blindly
	// retry: the unresolved attempt demands reconciliation first.
	if _, err := j.Begin(e, 2, "bob", now); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("unresolved operation must demand reconciliation: %v", err)
	}
	// Authoritative external state: the effect never happened.
	if _, err := j.Reconcile(op.LogicalID, ExternalState{Known: true, Applied: false}, now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Now exactly one retry proceeds — under the new epoch, same logical ID.
	retried, err := j.Begin(e, 2, "bob", now)
	if err != nil {
		t.Fatalf("post-reconciliation retry: %v", err)
	}
	if retried.LogicalID != op.LogicalID || len(retried.Attempts) != 2 {
		t.Fatalf("retry must adopt the same logical operation: %+v", retried)
	}
}

// Row: crash after the effect, before acknowledgment — reconciliation finds
// the exact effect and records Applied WITHOUT repeating it; the duplicate
// retry is then refused.
func TestJournal_CrashAfterEffect_ReconcilesToAppliedWithoutDuplicate(t *testing.T) {
	j := openJournal(t)
	e := testEffect()
	now := time.Now()

	op, err := j.Begin(e, 1, "alice", now)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// External query finds the PR that the crashed process actually created.
	rec, err := j.Reconcile(op.LogicalID, ExternalState{Known: true, Applied: true, Result: "acme/widgets#101"}, now)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rec.Status != StatusApplied || rec.Result != "acme/widgets#101" {
		t.Fatalf("reconciliation must record the found effect: %+v", rec)
	}
	// Any retry of the same desired effect — any owner, any epoch — is refused.
	if _, err := j.Begin(e, 9, "carol", now); !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("applied operation must never run twice: %v", err)
	}
}

// Row: ambiguous external state — the operation stays Unknown and no
// duplicate retry is authorized; an unrelated operation remains available.
func TestJournal_AmbiguousReconciliationBlocksRetryLocally(t *testing.T) {
	j := openJournal(t)
	e := testEffect()
	now := time.Now()
	op, _ := j.Begin(e, 1, "alice", now)
	if _, err := j.Reconcile(op.LogicalID, ExternalState{Known: false}, now); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := j.Begin(e, 2, "bob", now); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("unknown operation must not retry: %v", err)
	}
	// Positive control: a DISJOINT effect proceeds.
	other := testEffect()
	other.Subject = "acme/widgets#9"
	other.ClaimKey = TaskClaim("acme/widgets", "acme/widgets#9").Key()
	if _, err := j.Begin(other, 1, "bob", now); err != nil {
		t.Fatalf("unrelated effect must remain available: %v", err)
	}
}

// Row: duplicate/out-of-order results cannot regress terminal truth, and an
// old epoch cannot overwrite the current attempt's result.
func TestJournal_ResultsCannotRegressOrCrossEpochs(t *testing.T) {
	j := openJournal(t)
	e := testEffect()
	now := time.Now()
	op, _ := j.Begin(e, 1, "alice", now)

	// Uncertain first attempt, reconciled NotApplied, retried at epoch 2.
	if _, err := j.RecordResult(op.LogicalID, 1, StatusUnknown, "", now); err != nil {
		t.Fatalf("record unknown: %v", err)
	}
	if _, err := j.Reconcile(op.LogicalID, ExternalState{Known: true, Applied: false}, now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := j.Begin(e, 2, "bob", now); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// A LATE result from the fenced epoch-1 attempt may not overwrite.
	if _, err := j.RecordResult(op.LogicalID, 1, StatusApplied, "stale", now); !errors.Is(err, ErrResultRegression) {
		t.Fatalf("old epoch result must be refused: %v", err)
	}
	// Epoch 2 applies.
	rec, err := j.RecordResult(op.LogicalID, 2, StatusApplied, "acme/widgets#101", now)
	if err != nil || rec.Status != StatusApplied {
		t.Fatalf("current epoch result: %+v %v", rec, err)
	}
	// The identical duplicate acknowledgment deduplicates...
	if _, err := j.RecordResult(op.LogicalID, 2, StatusApplied, "acme/widgets#101", now); err != nil {
		t.Fatalf("duplicate ack must deduplicate: %v", err)
	}
	// ...but nothing regresses Applied.
	if _, err := j.RecordResult(op.LogicalID, 2, StatusUnknown, "", now); !errors.Is(err, ErrResultRegression) {
		t.Fatalf("terminal truth must not regress: %v", err)
	}
	if _, err := j.Reconcile(op.LogicalID, ExternalState{Known: true, Applied: false}, now); !errors.Is(err, ErrResultRegression) {
		t.Fatalf("disagreeing reconciliation is a refusal, not a rewrite: %v", err)
	}
	// A benign re-reconciliation of an Applied entry is a no-op.
	if rec, err := j.Reconcile(op.LogicalID, ExternalState{Known: true, Applied: true}, now); err != nil || rec.Status != StatusApplied {
		t.Fatalf("idempotent reconcile: %+v %v", rec, err)
	}
}

// Row: restart — journal and pending reconciliation reconstruct from durable
// state alone.
func TestJournal_RestartReconstructs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	e := testEffect()
	now := time.Now()
	op, _ := j.Begin(e, 1, "alice", now)

	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get(op.LogicalID)
	if !ok || got.Status != StatusPlanned || len(got.Attempts) != 1 {
		t.Fatalf("pending operation must reconstruct: %+v ok=%v", got, ok)
	}
	// The reconstructed entry still demands reconciliation before retry.
	if _, err := reopened.Begin(e, 2, "bob", now); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("restart must not authorize a blind retry: %v", err)
	}
}

// Row: corrupt journal refuses visibly; a forged entry (id not derived from
// its effect) refuses too.
func TestJournal_CorruptAndForgedRefuse(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := writeFile(corrupt, "]["); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(corrupt); err == nil {
		t.Fatal("corrupt journal must refuse")
	}

	forged := filepath.Join(dir, "forged.json")
	e := testEffect()
	body := fmt.Sprintf(`{"version":1,"operations":[{"logical_id":"op:forged","effect":{"outcome_key":%q,"desired_generation":1,"transition":%q,"subject":%q,"claim_key":%q,"kind":%q,"inputs":{"repo":"acme/widgets"}},"status":"Applied","attempts":[],"updated_at":"2026-08-20T00:00:00Z"}]}`,
		e.OutcomeKey, e.Transition, e.Subject, e.ClaimKey, e.Kind)
	if err := writeFile(forged, body); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(forged); err == nil {
		t.Fatal("an entry whose id does not derive from its effect must refuse")
	}
}

// Row: two journal writers race — the per-ID serialization produces one
// deterministic record; run under -race.
func TestJournal_ConcurrentWritersSerialize(t *testing.T) {
	j := openJournal(t)
	e := testEffect()
	now := time.Now()

	var wg sync.WaitGroup
	begins := make(chan Operation, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if op, err := j.Begin(e, uint64(n+1), fmt.Sprintf("h%d", n), now); err == nil {
				begins <- op
			}
		}(i)
	}
	wg.Wait()
	close(begins)
	count := 0
	for range begins {
		count++
	}
	if count != 1 {
		t.Fatalf("exactly one Begin must win for one logical operation, got %d", count)
	}
	got, ok := j.Get(e.LogicalID())
	if !ok || len(got.Attempts) != 1 {
		t.Fatalf("one deterministic record: %+v", got)
	}
}

func TestJournal_UnknownOperationRefusals(t *testing.T) {
	j := openJournal(t)
	now := time.Now()
	if _, err := j.RecordResult("op:none", 1, StatusApplied, "", now); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("unknown record: %v", err)
	}
	if _, err := j.Reconcile("op:none", ExternalState{Known: true}, now); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("unknown reconcile: %v", err)
	}
	if _, err := j.RecordResult("op:none", 1, "Bogus", "", now); !errors.Is(err, ErrInvalidEffect) {
		t.Fatalf("bogus status: %v", err)
	}
	if _, err := j.Begin(Effect{}, 1, "alice", now); !errors.Is(err, ErrInvalidEffect) {
		t.Fatalf("invalid effect: %v", err)
	}
}

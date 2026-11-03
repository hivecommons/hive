package mutation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

const journalProgressDeadline = 3 * time.Second

func waitJournalSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(journalProgressDeadline):
		t.Fatal("journal transition did not make progress")
	}
}

func TestJournalSlowWriterPublishesOnlyCommittedOrderedSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	first := crossProcessEffect("acme/widgets#1", "first")
	second := crossProcessEffect("acme/widgets#2", "second")
	entered := make(chan struct{})
	release := make(chan struct{})
	secondEntered := make(chan struct{})
	secondRelease := make(chan struct{})
	writes := 0
	j.writeSnapshot = func(path string, data []byte) error {
		writes++
		switch writes {
		case 1:
			close(entered)
			<-release
		case 2:
			// A later image cannot overtake the first physical replacement.
			disk, err := OpenJournal(path)
			if err != nil {
				return err
			}
			if _, ok := disk.Get(first.LogicalID()); !ok {
				return errors.New("second writer overtook first commit")
			}
			close(secondEntered)
			<-secondRelease
		}
		return replaceJournalSnapshot(path, data)
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := j.Begin(first, 1, "a", time.Now()); firstDone <- err }()
	waitJournalSignal(t, entered)
	go func() { _, err := j.Begin(second, 1, "b", time.Now()); secondDone <- err }()
	// This is the actual Get used by reconciliation and replay, not merely
	// a proxy mutex check. Neither pending intent may be exposed as durable.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		if _, ok := j.Get(first.LogicalID()); ok {
			t.Error("uncommitted first intent leaked to reader")
		}
	}()
	waitJournalSignal(t, readDone)
	select {
	case err := <-secondDone:
		t.Fatalf("later dirty state falsely acknowledged: %v", err)
	default:
	}
	close(release)
	waitJournalSignal(t, secondEntered)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Get(first.LogicalID()); !ok {
		t.Fatal("committed first intent unavailable while next writer is slow")
	}
	if _, ok := j.Get(second.LogicalID()); ok {
		t.Fatal("older commit acknowledged newer pending state")
	}
	close(secondRelease)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Effect{first, second} {
		if op, ok := reopened.Get(e.LogicalID()); !ok || op.Status != StatusPlanned {
			t.Fatalf("restart lost unresolved intent: %+v, %v", op, ok)
		}
	}
}

func TestJournalFailedWriterDoesNotLoseQueuedOrRetriedOperations(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := crossProcessEffect("acme/widgets#1", "first")
	second := crossProcessEffect("acme/widgets#2", "second")
	entered, release := make(chan struct{}), make(chan struct{})
	writes := 0
	j.writeSnapshot = func(path string, data []byte) error {
		writes++
		if writes == 1 {
			close(entered)
			<-release
			return os.ErrPermission
		}
		return replaceJournalSnapshot(path, data)
	}
	failed, queued := make(chan error, 1), make(chan error, 1)
	go func() { _, err := j.Begin(first, 1, "a", time.Now()); failed <- err }()
	waitJournalSignal(t, entered)
	go func() { _, err := j.Begin(second, 1, "b", time.Now()); queued <- err }()
	close(release)
	if err := <-failed; !errors.Is(err, os.ErrPermission) {
		t.Fatalf("write failure hidden: %v", err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Get(first.LogicalID()); ok {
		t.Fatal("failed write published an intent")
	}
	if _, err := j.Begin(first, 1, "a", time.Now()); err != nil {
		t.Fatalf("failed intent not retryable: %v", err)
	}
	reopened, err := OpenJournal(j.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Effect{first, second} {
		if _, ok := reopened.Get(e.LogicalID()); !ok {
			t.Fatal("queued or retried operation lost on restart")
		}
	}
}

func TestExecutorCanceledJournalAdmissionNeverInvokesExternalEffect(t *testing.T) {
	x, claim, now := newExecutor(t, proof.ModeEnforce)
	entry, err := x.Ledger.Acquire(claim, "alice", ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	x.Journal.writer <- struct{}{}
	defer func() { <-x.Journal.writer }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	if _, err := x.ExecuteContext(ctx, testEffect(), entry.Epoch, "alice", func() (string, error) {
		calls++
		return "must-not-run", nil
	}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled optional caller executed through a busy journal: err=%v calls=%d", err, calls)
	}
}

func TestExecutorCancellationDuringIntentLeavesRestartReconciliation(t *testing.T) {
	x, claim, now := newExecutor(t, proof.ModeEnforce)
	entry, err := x.Ledger.Acquire(claim, "alice", ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x.Journal.writeSnapshot = func(path string, data []byte) error {
		cancel()
		return replaceJournalSnapshot(path, data)
	}
	calls := 0
	e := testEffect()
	if _, err := x.ExecuteContext(ctx, e, entry.Epoch, "alice", func() (string, error) {
		calls++
		return "must-not-run", nil
	}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("effect ran after cancellation during intent: err=%v calls=%d", err, calls)
	}
	reopened, err := OpenJournal(x.Journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get(e.LogicalID()); !ok || got.Status != StatusPlanned {
		t.Fatalf("restart lost unresolved durable intent: %+v %v", got, ok)
	}
	if _, err := reopened.Begin(e, entry.Epoch, "alice", now); !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("canceled intent blindly replayed after restart: %v", err)
	}
}

func TestJournalCancellationWaitsOnlyForAdmissionNotOptimisticAcknowledgement(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := testEffect()
	op, err := j.Begin(e, 1, "a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	j.writeSnapshot = func(path string, data []byte) error {
		close(entered)
		<-release
		return replaceJournalSnapshot(path, data)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := j.RecordResultContext(ctx, op.LogicalID, 1, StatusApplied, "applied", time.Now())
		done <- err
	}()
	waitJournalSignal(t, entered)
	cancel()
	if got, _ := j.Get(op.LogicalID); got.Status != StatusPlanned {
		t.Fatal("canceled writer exposed an uncommitted acknowledgment")
	}
	if _, err := j.BeginContext(ctx, crossProcessEffect("acme/widgets#2", "second"), 1, "b", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued caller ignored cancellation: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("physical replacement was optimistically acknowledged: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	j.writeSnapshot = func(string, []byte) error { t.Error("unchanged replay rewrote journal"); return nil }
	if _, err := j.RecordResult(op.LogicalID, 1, StatusApplied, "applied", time.Now()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(j.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get(op.LogicalID); got.Status != StatusApplied || got.Result != "applied" {
		t.Fatalf("cancellation lost the completed durable result: %+v", got)
	}
}

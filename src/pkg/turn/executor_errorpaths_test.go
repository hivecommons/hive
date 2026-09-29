package turn

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDoAlreadyDoneShortCircuitsWithoutReplay covers the branch of
// JournaledExecutor.Do that finds a journal entry already settled as
// succeeded: it must return the recorded ref and never call the sink again.
func TestDoAlreadyDoneShortCircuitsWithoutReplay(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, time.Now())
	env.Journal.settle(op.IdempotencyKey, OpSucceeded, "remote-existing", "", time.Now())

	remote := newFakeRemote()
	executor := &JournaledExecutor{Sink: remote, Reconciler: remote, Persister: &killStore{}}

	result, err := executor.Do(context.Background(), &env, op)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !result.AlreadyExisted || result.ExternalRef != "remote-existing" {
		t.Fatalf("result = %+v, want AlreadyExisted with the recorded ref", result)
	}
	if remote.calls[effectID(op.Intent)] != 0 || remote.reconcileCalls != 0 {
		t.Fatalf("already-done effect was replayed or reconciled: calls=%d reconcileCalls=%d",
			remote.calls[effectID(op.Intent)], remote.reconcileCalls)
	}
}

// failingReconciler always returns an error, exercising the "reconcile %s: %w"
// wrap in Do.
type failingReconciler struct {
	err error
}

func (f *failingReconciler) Reconcile(context.Context, OpIntent) (string, bool, error) {
	return "", false, f.err
}

func TestDoWrapsReconcilerError(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, time.Now())

	remote := newFakeRemote()
	reconcileErr := errors.New("upstream unavailable")
	executor := &JournaledExecutor{Sink: remote, Reconciler: &failingReconciler{err: reconcileErr}, Persister: &killStore{}}

	_, err := executor.Do(context.Background(), &env, op)
	if err == nil || !errors.Is(err, reconcileErr) {
		t.Fatalf("Do error = %v, want it to wrap %v", err, reconcileErr)
	}
	if remote.calls[effectID(op.Intent)] != 0 {
		t.Fatal("effect was performed after a failed reconcile")
	}
}

// TestDoReconciledFoundPersistFailure covers the persist call made right after
// a reconciler finds a prior effect (the "found" branch), distinct from the
// persist call made after a fresh Perform.
func TestDoReconciledFoundPersistFailure(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, time.Now())

	remote := newFakeRemote()
	remote.refs[effectID(op.Intent)] = "remote-found"
	store := &killStore{killAt: 1}
	executor := &JournaledExecutor{Sink: remote, Reconciler: remote, Persister: store}

	_, err := executor.Do(context.Background(), &env, op)
	if !errors.Is(err, errKilled) {
		t.Fatalf("Do error = %v, want errKilled", err)
	}
	if remote.reconcileCalls != 1 {
		t.Fatalf("reconcileCalls = %d, want 1", remote.reconcileCalls)
	}
}

// TestDoFailedEffectPersistFailureJoinsErrors covers the errors.Join branch:
// the effect itself fails AND the subsequent settle-persist also fails.
func TestDoFailedEffectPersistFailureJoinsErrors(t *testing.T) {
	env := testEnvelope()
	op := bindPlan(env.SessionID, testPlan()).Operations[0]

	remote := newFakeRemote()
	effectErr := errors.New("remote rejected")
	remote.fail[effectID(op.Intent)] = effectErr
	// killAt=2: the first persist (recording intent) succeeds, the second
	// persist (after the failed settle) is the one that must fail.
	store := &killStore{killAt: 2}
	executor := &JournaledExecutor{Sink: remote, Persister: store}

	_, err := executor.Do(context.Background(), &env, op)
	if !errors.Is(err, effectErr) {
		t.Fatalf("Do error = %v, want it to wrap %v", err, effectErr)
	}
	if !errors.Is(err, errKilled) {
		t.Fatalf("Do error = %v, want it to also wrap errKilled", err)
	}
}

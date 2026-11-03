package effects

import (
	"context"
	"errors"
	"testing"
)

type reconcilingBoundary struct {
	stubBoundary
	gotClaim Claim
	gotState ExternalState
	calls    int
	err      error
}

func (r *reconcilingBoundary) Reconcile(_ context.Context, claim Claim, state ExternalState) error {
	r.calls++
	r.gotClaim = claim
	r.gotState = state
	return r.err
}

func TestNotAppliedNilReturnsNil(t *testing.T) {
	if err := NotApplied(nil); err != nil {
		t.Fatalf("NotApplied(nil) = %v, want nil", err)
	}
}

type codedError struct{ code int }

func (e *codedError) Error() string { return "coded" }

func TestNotAppliedWrapsAndKeepsChain(t *testing.T) {
	sentinel := errors.New("refused")
	inner := &codedError{code: 405}
	chain := errors.Join(sentinel, inner)
	wrapped := NotApplied(chain)
	if !errors.Is(wrapped, ErrNotApplied) {
		t.Fatalf("errors.Is(wrapped, ErrNotApplied) = false")
	}
	if !errors.Is(wrapped, chain) || !errors.Is(wrapped, sentinel) {
		t.Fatalf("wrapped does not resolve original chain")
	}
	if wrapped.Error() != chain.Error() {
		t.Fatalf("Error() = %q, want %q", wrapped.Error(), chain.Error())
	}
	var ce *codedError
	if !errors.As(wrapped, &ce) || ce.code != 405 {
		t.Fatalf("errors.As did not find *codedError: %v", ce)
	}
	if errors.Is(wrapped, ErrNeedsReconciliation) {
		t.Fatalf("wrapped unexpectedly matches ErrNeedsReconciliation")
	}
}

func TestReconcileNonReconcilerBoundariesReturnNil(t *testing.T) {
	claim := Claim{Repo: "o/r", Kind: KindPullRequestMerge, Target: "pr/1"}
	if err := Reconcile(context.Background(), nil, claim, ExternalState{}); err != nil {
		t.Fatalf("Reconcile(nil) = %v, want nil", err)
	}
	if err := Reconcile(context.Background(), NoopBoundary{}, claim, ExternalState{Applied: true}); err != nil {
		t.Fatalf("Reconcile(NoopBoundary) = %v, want nil", err)
	}
}

func TestReconcileDelegatesToReconciler(t *testing.T) {
	want := errors.New("reconcile failed")
	fake := &reconcilingBoundary{err: want}
	claim := Claim{Repo: "o/r", Kind: KindPullRequestMerge, Target: "pr/1"}
	state := ExternalState{Applied: true, Provenance: "sha123"}
	err := Reconcile(context.Background(), fake, claim, state)
	if !errors.Is(err, want) {
		t.Fatalf("Reconcile() = %v, want %v", err, want)
	}
	if fake.calls != 1 || fake.gotClaim.Target != claim.Target || fake.gotState != state {
		t.Fatalf("fake got calls=%d claim=%+v state=%+v", fake.calls, fake.gotClaim, fake.gotState)
	}
}

func TestLoggingBoundaryReconcileDelegates(t *testing.T) {
	want := errors.New("nope")
	fake := &reconcilingBoundary{err: want}
	claim := Claim{Repo: "o/r", Kind: KindPullRequestMerge, Target: "pr/2"}
	state := ExternalState{Provenance: "p"}
	err := LoggingBoundary{Next: fake}.Reconcile(context.Background(), claim, state)
	if !errors.Is(err, want) {
		t.Fatalf("Reconcile() = %v, want %v", err, want)
	}
	if fake.calls != 1 || fake.gotClaim.Target != claim.Target || fake.gotState != state {
		t.Fatalf("fake got calls=%d claim=%+v state=%+v", fake.calls, fake.gotClaim, fake.gotState)
	}
}

func TestLoggingBoundaryReconcileNonReconcilerNext(t *testing.T) {
	for name, next := range map[string]Boundary{"nil": nil, "noop": NoopBoundary{}} {
		t.Run(name, func(t *testing.T) {
			if err := (LoggingBoundary{Next: next}).Reconcile(context.Background(), Claim{}, ExternalState{}); err != nil {
				t.Fatalf("Reconcile() = %v, want nil", err)
			}
		})
	}
}

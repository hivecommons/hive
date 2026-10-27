package effects

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedBoundary refuses with ErrNeedsReconciliation until it is reconciled
// or expired, then runs the effect.
type scriptedBoundary struct {
	needs     bool
	reconErr  error
	expireOK  bool
	expireErr error
	executes  int
	recons    int
	expires   int
	gotState  ExternalState
	gotTTL    time.Duration
}

func (s *scriptedBoundary) Execute(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	s.executes++
	if s.needs {
		return Result{}, ErrNeedsReconciliation
	}
	return effect(ctx)
}

func (s *scriptedBoundary) Reconcile(_ context.Context, _ Claim, state ExternalState) error {
	s.recons++
	s.gotState = state
	if s.reconErr == nil {
		s.needs = false
	}
	return s.reconErr
}

func (s *scriptedBoundary) ExpireUnresolved(_ context.Context, _ Claim, ttl time.Duration) (bool, error) {
	s.expires++
	s.gotTTL = ttl
	if s.expireErr != nil {
		return false, s.expireErr
	}
	if s.expireOK {
		s.needs = false
	}
	return s.expireOK, nil
}

var reconcilingClaim = Claim{Repo: "o/r", Kind: KindIssueCreate, Target: "t"}

func okEffect(ran *int) func(context.Context) (Result, error) {
	return func(context.Context) (Result, error) {
		*ran++
		return Result{Provenance: "ran"}, nil
	}
}

func TestExecuteReconcilingPassesThroughWithoutRefusal(t *testing.T) {
	ran := 0
	b := &scriptedBoundary{}
	res, err := ExecuteReconciling(context.Background(), b, reconcilingClaim, nil, 0, okEffect(&ran))
	if err != nil || res.Provenance != "ran" || ran != 1 || b.executes != 1 || b.recons != 0 || b.expires != 0 {
		t.Fatalf("got %+v, %v ran=%d executes=%d recons=%d expires=%d", res, err, ran, b.executes, b.recons, b.expires)
	}
}

func TestExecuteReconcilingPassesThroughOtherErrors(t *testing.T) {
	want := errors.New("boom")
	_, err := ExecuteReconciling(context.Background(), stubBoundary{err: want}, reconcilingClaim, nil, 0,
		func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestExecuteReconcilingObservedStateReplays(t *testing.T) {
	ran := 0
	b := &scriptedBoundary{needs: true}
	state := ExternalState{Applied: false, Provenance: "obs"}
	res, err := ExecuteReconciling(context.Background(), b, reconcilingClaim,
		func(context.Context) (ExternalState, error) { return state, nil }, 0, okEffect(&ran))
	if err != nil || res.Provenance != "ran" || ran != 1 {
		t.Fatalf("got %+v, %v ran=%d", res, err, ran)
	}
	if b.recons != 1 || b.gotState != state || b.executes != 2 || b.expires != 0 {
		t.Fatalf("recons=%d state=%+v executes=%d expires=%d", b.recons, b.gotState, b.executes, b.expires)
	}
}

func TestExecuteReconcilingReconcileFailureKeepsRefusal(t *testing.T) {
	b := &scriptedBoundary{needs: true, reconErr: errors.New("journal write failed")}
	_, err := ExecuteReconciling(context.Background(), b, reconcilingClaim,
		func(context.Context) (ExternalState, error) { return ExternalState{}, nil }, 0,
		func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, ErrNeedsReconciliation) || !strings.Contains(err.Error(), "reconciliation failed: journal write failed") {
		t.Fatalf("err = %v", err)
	}
	if b.executes != 1 || b.expires != 0 {
		t.Fatalf("executes=%d expires=%d", b.executes, b.expires)
	}
}

func TestExecuteReconcilingNoObserverExpiresWithDefaultTTL(t *testing.T) {
	ran := 0
	b := &scriptedBoundary{needs: true, expireOK: true}
	res, err := ExecuteReconciling(context.Background(), b, reconcilingClaim, nil, 0, okEffect(&ran))
	if err != nil || res.Provenance != "ran" || ran != 1 {
		t.Fatalf("got %+v, %v ran=%d", res, err, ran)
	}
	if b.gotTTL != DefaultUnresolvedTTL || b.expires != 1 || b.recons != 0 || b.executes != 2 {
		t.Fatalf("ttl=%s expires=%d recons=%d executes=%d", b.gotTTL, b.expires, b.recons, b.executes)
	}
}

func TestExecuteReconcilingObserveFailureExpiresWithGivenTTL(t *testing.T) {
	ran := 0
	b := &scriptedBoundary{needs: true, expireOK: true}
	_, err := ExecuteReconciling(context.Background(), b, reconcilingClaim,
		func(context.Context) (ExternalState, error) { return ExternalState{}, errors.New("api down") },
		5*time.Minute, okEffect(&ran))
	if err != nil || ran != 1 || b.gotTTL != 5*time.Minute || b.recons != 0 {
		t.Fatalf("err=%v ran=%d ttl=%s recons=%d", err, ran, b.gotTTL, b.recons)
	}
}

func TestExecuteReconcilingYoungEntryKeepsRefusal(t *testing.T) {
	b := &scriptedBoundary{needs: true}
	_, err := ExecuteReconciling(context.Background(), b, reconcilingClaim,
		func(context.Context) (ExternalState, error) { return ExternalState{}, errors.New("api down") },
		time.Minute, func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, ErrNeedsReconciliation) || !strings.Contains(err.Error(), "reconciliation lookup failed: api down") {
		t.Fatalf("err = %v", err)
	}
	if b.executes != 1 {
		t.Fatalf("executes = %d, want 1", b.executes)
	}
}

func TestExecuteReconcilingExpiryErrorReported(t *testing.T) {
	b := &scriptedBoundary{needs: true, expireErr: errors.New("disk full")}
	_, err := ExecuteReconciling(context.Background(), b, reconcilingClaim, nil, time.Minute,
		func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, ErrNeedsReconciliation) ||
		!strings.Contains(err.Error(), "no external observation for "+KindIssueCreate) ||
		!strings.Contains(err.Error(), "expiry failed: disk full") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteReconcilingNonJournalingBoundaryKeepsRefusal(t *testing.T) {
	_, err := ExecuteReconciling(context.Background(), stubBoundary{err: ErrNeedsReconciliation}, reconcilingClaim, nil, 0,
		func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("err = %v", err)
	}
}

type stubLookup struct {
	stubBoundary
	res Result
	ok  bool
}

func (s stubLookup) LookupApplied(context.Context, Claim) (Result, bool) { return s.res, s.ok }

func TestLookupAppliedForwardsOnlyToLookups(t *testing.T) {
	ctx := context.Background()
	if res, ok := LookupApplied(ctx, nil, reconcilingClaim); ok || res != (Result{}) {
		t.Fatalf("nil boundary: (%+v, %v)", res, ok)
	}
	if _, ok := LookupApplied(ctx, NoopBoundary{}, reconcilingClaim); ok {
		t.Fatal("NoopBoundary must not report applied")
	}
	res, ok := LookupApplied(ctx, stubLookup{res: Result{Provenance: "p"}, ok: true}, reconcilingClaim)
	if !ok || res.Provenance != "p" {
		t.Fatalf("got (%+v, %v)", res, ok)
	}
}

package mutation

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

// #10536: a definite external refusal must not strand the logical operation
// as Unknown, and an operation already left Unknown must be resolvable from
// authoritative external state through the boundary.

func TestBoundaryEffectErrorClassification(t *testing.T) {
	refusal := errors.New("405 Pull Request is not mergeable")
	tests := []struct {
		name       string
		effectErr  error
		wantStatus string
		// wantRetry: a second Execute of the same logical operation runs the
		// effect again instead of refusing with ErrNeedsReconciliation.
		wantRetry bool
	}{
		{"definite refusal records NotApplied", effects.NotApplied(refusal), StatusNotApplied, true},
		{"ambiguous error records Unknown", refusal, StatusUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBoundary(t, "enforce")
			claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge, Target: "7", Inputs: map[string]string{"method": "squash"}}
			_, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{}, tt.effectErr
			})
			if !errors.Is(err, refusal) {
				t.Fatalf("first Execute err = %v, want the effect's own error", err)
			}
			if tt.wantRetry && err.Error() != refusal.Error() {
				t.Fatalf("definite refusal err = %q, want the original message %q unchanged", err, refusal)
			}
			op, ok := b.Executor.Journal.Get(boundaryEffect(claim).LogicalID())
			if !ok || op.Status != tt.wantStatus {
				t.Fatalf("journal = (%+v, %v), want status %s", op, ok, tt.wantStatus)
			}
			called := false
			_, err = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				called = true
				return effects.Result{Provenance: "sha"}, nil
			})
			if tt.wantRetry {
				if err != nil || !called {
					t.Fatalf("retry after definite refusal = (%v, called=%v), want the effect to run", err, called)
				}
				return
			}
			if called || !errors.Is(err, effects.ErrNeedsReconciliation) || !errors.Is(err, ErrNeedsReconciliation) {
				t.Fatalf("retry after ambiguous error = (%v, called=%v), want ErrNeedsReconciliation without the effect", err, called)
			}
		})
	}
}

func TestBoundaryReconcileResolvesUnknown(t *testing.T) {
	tests := []struct {
		name           string
		state          effects.ExternalState
		wantStatus     string
		wantEffectRuns bool
		wantProvenance string
	}{
		{"applied externally replays as idempotent success", effects.ExternalState{Applied: true, Provenance: "mergesha"}, StatusApplied, false, "mergesha"},
		{"not applied externally authorizes a retry", effects.ExternalState{Applied: false}, StatusNotApplied, true, "retried"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBoundary(t, "enforce")
			claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge, Target: "7", Inputs: map[string]string{"method": "squash"}}
			if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{}, errors.New("connection reset")
			}); err == nil {
				t.Fatal("ambiguous effect error must surface")
			}
			if err := effects.Reconcile(context.Background(), b, claim, tt.state); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			op, _ := b.Executor.Journal.Get(boundaryEffect(claim).LogicalID())
			if op.Status != tt.wantStatus {
				t.Fatalf("journal status = %s, want %s", op.Status, tt.wantStatus)
			}
			called := false
			res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				called = true
				return effects.Result{Provenance: "retried"}, nil
			})
			if err != nil || called != tt.wantEffectRuns || res.Provenance != tt.wantProvenance {
				t.Fatalf("Execute after reconcile = (%+v, %v, called=%v), want provenance %q called=%v",
					res, err, called, tt.wantProvenance, tt.wantEffectRuns)
			}
		})
	}
}

func TestBoundaryReconcileOffModeIsNoop(t *testing.T) {
	b := testBoundary(t, "off")
	if err := b.Reconcile(context.Background(), effects.Claim{}, effects.ExternalState{Applied: true}); err != nil {
		t.Fatalf("off-mode Reconcile = %v, want nil", err)
	}
}

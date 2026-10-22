package mutation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// A journaled op that stays Unknown is refused on every retry; once it has
// been unresolved for StuckAfter the boundary reports it through OnStuck
// (with the op and its age) and OnResolved fires when reconciliation lands.
func TestBoundaryReportsStuckUnknownOpAndResolvedOnReconcile(t *testing.T) {
	b := testBoundary(t, "enforce")
	now := time.Unix(100, 0)
	b.Executor.Now = func() time.Time { return now }
	var stuck []Operation
	var stuckAges []time.Duration
	var resolved []string
	b.OnStuck = func(op Operation, age time.Duration) { stuck = append(stuck, op); stuckAges = append(stuckAges, age) }
	b.OnResolved = func(id string) { resolved = append(resolved, id) }
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "title"}
	boom := errors.New("boom")

	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("first execute err = %v", err)
	}
	// Retry straight away: refused, but not yet stuck.
	now = now.Add(5 * time.Minute)
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{Provenance: "x"}, nil }); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("retry err = %v, want needs-reconciliation", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("reported stuck at 5m: %+v", stuck)
	}
	now = now.Add(DefaultStuckAfter)
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{Provenance: "x"}, nil }); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("late retry err = %v", err)
	}
	if len(stuck) != 1 || stuck[0].Status != StatusUnknown || stuck[0].Effect.Kind != effects.KindIssueCreate || stuck[0].Effect.Inputs["repo"] != "acme/widget" {
		t.Fatalf("stuck = %+v", stuck)
	}
	if stuckAges[0] < DefaultStuckAfter {
		t.Fatalf("age = %s, want ≥ %s", stuckAges[0], DefaultStuckAfter)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved before reconcile: %v", resolved)
	}
	if err := b.Reconcile(context.Background(), claim, effects.ExternalState{Applied: true, Provenance: "https://x/1"}); err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0] != stuck[0].LogicalID {
		t.Fatalf("resolved = %v, want %q", resolved, stuck[0].LogicalID)
	}
	// Idempotent replay of the applied op also counts as resolved.
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{}, nil }); err != nil {
		t.Fatalf("replay err = %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved after replay = %v", resolved)
	}
}

func TestBoundaryStuckHooksHonourCustomThresholdAndSuccess(t *testing.T) {
	b := testBoundary(t, "enforce")
	now := time.Unix(100, 0)
	b.Executor.Now = func() time.Time { return now }
	b.StuckAfter = time.Minute
	stuck := 0
	var resolved []string
	b.OnStuck = func(Operation, time.Duration) { stuck++ }
	b.OnResolved = func(id string) { resolved = append(resolved, id) }
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "ok"}
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{Provenance: "p"}, nil }); err != nil {
		t.Fatal(err)
	}
	if stuck != 0 || len(resolved) != 1 {
		t.Fatalf("success: stuck=%d resolved=%v", stuck, resolved)
	}
	bad := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "bad"}
	_, _ = b.Execute(context.Background(), bad, func(context.Context) (effects.Result, error) { return effects.Result{}, errors.New("boom") })
	now = now.Add(2 * time.Minute)
	_, _ = b.Execute(context.Background(), bad, func(context.Context) (effects.Result, error) { return effects.Result{}, nil })
	if stuck != 1 {
		t.Fatalf("custom threshold: stuck=%d, want 1", stuck)
	}
}

// Without hooks the boundary behaves exactly as before.
func TestBoundaryStuckHooksOptional(t *testing.T) {
	b := testBoundary(t, "enforce")
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "t"}
	_, _ = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{}, errors.New("boom") })
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { return effects.Result{}, nil }); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("err = %v", err)
	}
}

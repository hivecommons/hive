package mutation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// hivecommons/hive#17693: an issue create left Unknown by an ambiguous
// response, whose caller only ever calls Execute (no Reconcile/Expire of its
// own), was refused and reported stuck every cycle for 15h. Past the kind's
// unresolved TTL the refused call must expire the op to NotApplied, clear the
// stuck alert and — without running the effect in that call — let the
// caller's next attempt create.
func TestBoundaryExpiresRefusedUnknownIssueCreatePastTTL(t *testing.T) {
	b := testBoundary(t, "enforce")
	now := time.Unix(100, 0)
	b.Executor.Now = func() time.Time { return now }
	stuck := 0
	var resolved []string
	b.OnStuck = func(Operation, time.Duration) { stuck++ }
	b.OnResolved = func(id string) { resolved = append(resolved, id) }
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "advisory"}
	id := boundaryEffect(claim).LogicalID()
	mustNotRun := func(context.Context) (effects.Result, error) {
		t.Fatal("effect must not run on a refused attempt")
		return effects.Result{}, nil
	}

	ambiguous := errors.New("502 Bad Gateway")
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{}, ambiguous
	}); !errors.Is(err, ambiguous) {
		t.Fatalf("first execute err = %v", err)
	}

	// Inside the TTL: refused and reported stuck, still Unknown.
	now = now.Add(DefaultStuckAfter)
	_, err := b.Execute(context.Background(), claim, mustNotRun)
	if !errors.Is(err, effects.ErrNeedsReconciliation) || errors.Is(err, effects.ErrUnresolvedExpired) {
		t.Fatalf("inside TTL err = %v, want a plain refusal", err)
	}
	if op, _ := b.Executor.Journal.Get(id); op.Status != StatusUnknown || stuck != 1 {
		t.Fatalf("inside TTL: status=%s stuck=%d, want Unknown and 1", op.Status, stuck)
	}

	// Past the TTL: still refused this call, but expired and resolved.
	now = now.Add(effects.DefaultUnresolvedTTL)
	_, err = b.Execute(context.Background(), claim, mustNotRun)
	if !errors.Is(err, effects.ErrNeedsReconciliation) || !errors.Is(err, effects.ErrUnresolvedExpired) {
		t.Fatalf("past TTL err = %v, want refusal carrying ErrUnresolvedExpired", err)
	}
	if op, _ := b.Executor.Journal.Get(id); op.Status != StatusNotApplied {
		t.Fatalf("past TTL status = %s, want NotApplied", op.Status)
	}
	if stuck != 1 {
		t.Fatalf("stuck = %d after expiry, want no further report", stuck)
	}
	if len(resolved) != 1 || resolved[0] != id {
		t.Fatalf("resolved = %v, want [%s]", resolved, id)
	}

	// The next attempt is a fresh one and runs the effect.
	ran := false
	res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		ran = true
		return effects.Result{Provenance: "https://github.example/acme/widget/issues/9"}, nil
	})
	if err != nil || !ran || res.Provenance == "" {
		t.Fatalf("retry = (%+v, %v, ran=%v), want the create to run", res, err, ran)
	}
	if stuck != 1 {
		t.Fatalf("stuck = %d after successful retry", stuck)
	}
}

// Reconciliation still wins inside the TTL: an issue found externally
// resolves the op Applied (replayed without creating), one definitively
// absent releases a retry.
func TestBoundaryReconcileResolvesUnknownIssueCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  effects.ExternalState
		status string
		runs   bool
	}{
		{"found", effects.ExternalState{Applied: true, Provenance: "https://github.example/acme/widget/issues/7"}, StatusApplied, false},
		{"absent", effects.ExternalState{Applied: false}, StatusNotApplied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBoundary(t, "enforce")
			claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "finding"}
			_, _ = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{}, errors.New("connection reset")
			})
			if err := b.Reconcile(context.Background(), claim, tc.state); err != nil {
				t.Fatal(err)
			}
			if op, _ := b.Executor.Journal.Get(boundaryEffect(claim).LogicalID()); op.Status != tc.status {
				t.Fatalf("status = %s, want %s", op.Status, tc.status)
			}
			ran := false
			res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				ran = true
				return effects.Result{Provenance: "https://github.example/acme/widget/issues/8"}, nil
			})
			if err != nil || ran != tc.runs {
				t.Fatalf("after reconcile: err=%v ran=%v, want ran=%v", err, ran, tc.runs)
			}
			if !tc.runs && res.Provenance != tc.state.Provenance {
				t.Fatalf("replay provenance = %q, want %q", res.Provenance, tc.state.Provenance)
			}
		})
	}
}

// Kinds without an unresolved TTL keep refusing (and reporting stuck) until
// an explicit Reconcile, however old the entry; an explicit empty map
// disables the default for issue creates too.
func TestBoundaryUnresolvedTTLOnlyForConfiguredKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		ttls map[string]time.Duration
	}{
		{"merge default", effects.KindPullRequestMerge, nil},
		{"issue create disabled", effects.KindIssueCreate, map[string]time.Duration{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBoundary(t, "enforce")
			now := time.Unix(100, 0)
			b.Executor.Now = func() time.Time { return now }
			b.UnresolvedTTL = tc.ttls
			stuck := 0
			b.OnStuck = func(Operation, time.Duration) { stuck++ }
			claim := effects.Claim{Repo: "acme/widget", Kind: tc.kind, Target: "7"}
			_, _ = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{}, errors.New("timeout")
			})
			now = now.Add(48 * time.Hour)
			_, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				t.Fatal("effect must not run")
				return effects.Result{}, nil
			})
			if !errors.Is(err, effects.ErrNeedsReconciliation) || errors.Is(err, effects.ErrUnresolvedExpired) {
				t.Fatalf("err = %v, want a plain refusal", err)
			}
			if op, _ := b.Executor.Journal.Get(boundaryEffect(claim).LogicalID()); op.Status != StatusUnknown || stuck != 1 {
				t.Fatalf("status=%s stuck=%d, want Unknown and 1", op.Status, stuck)
			}
		})
	}
}

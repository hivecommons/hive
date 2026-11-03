package mutation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// An operation left Unknown that nothing can reconcile must not fence every
// retry forever: past the caller's TTL the boundary expires it to NotApplied
// so the same logical operation may run again. Inside the TTL, and for
// Applied entries, nothing changes.
func TestBoundaryExpireUnresolved(t *testing.T) {
	now := time.Unix(1_000, 0)
	b := testBoundary(t, "enforce")
	b.Executor.Now = func() time.Time { return now }
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "finding-1"}
	ambiguous := errors.New("502 Bad Gateway")
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{}, ambiguous
	}); !errors.Is(err, ambiguous) {
		t.Fatalf("first Execute err = %v", err)
	}
	id := boundaryEffect(claim).LogicalID()
	if op, _ := b.Executor.Journal.Get(id); op.Status != StatusUnknown {
		t.Fatalf("status = %s, want Unknown", op.Status)
	}

	const ttl = time.Hour
	now = now.Add(ttl / 2)
	if expired, err := effects.ExpireUnresolved(context.Background(), b, claim, ttl); err != nil || expired {
		t.Fatalf("inside TTL: (expired=%v, err=%v), want untouched", expired, err)
	}
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		t.Fatal("effect must not run while the entry is unresolved")
		return effects.Result{}, nil
	}); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("inside TTL Execute err = %v, want ErrNeedsReconciliation", err)
	}

	now = now.Add(ttl)
	if expired, err := effects.ExpireUnresolved(context.Background(), b, claim, ttl); err != nil || !expired {
		t.Fatalf("past TTL: (expired=%v, err=%v), want expired", expired, err)
	}
	if op, _ := b.Executor.Journal.Get(id); op.Status != StatusNotApplied {
		t.Fatalf("status = %s, want NotApplied", op.Status)
	}
	ran := false
	res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		ran = true
		return effects.Result{Provenance: "https://github.example/acme/widget/issues/9"}, nil
	})
	if err != nil || !ran || res.Provenance == "" {
		t.Fatalf("retry after expiry = (%+v, %v, ran=%v), want the effect to run", res, err, ran)
	}

	// Applied is terminal: expiry never touches it, however old.
	now = now.Add(24 * ttl)
	if expired, err := effects.ExpireUnresolved(context.Background(), b, claim, ttl); err != nil || expired {
		t.Fatalf("applied entry: (expired=%v, err=%v), want untouched", expired, err)
	}
	// Nothing journaled, nothing to expire; a non-positive TTL is a no-op.
	if expired, err := effects.ExpireUnresolved(context.Background(), b, effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "never"}, ttl); err != nil || expired {
		t.Fatalf("unknown claim: (expired=%v, err=%v)", expired, err)
	}
	if expired, err := effects.ExpireUnresolved(context.Background(), b, claim, 0); err != nil || expired {
		t.Fatalf("zero TTL: (expired=%v, err=%v)", expired, err)
	}
	if expired, err := effects.ExpireUnresolved(context.Background(), testBoundary(t, "off"), claim, ttl); err != nil || expired {
		t.Fatalf("off mode: (expired=%v, err=%v)", expired, err)
	}
	if expired, err := effects.ExpireUnresolved(context.Background(), effects.NoopBoundary{}, claim, ttl); err != nil || expired {
		t.Fatalf("non-journaling boundary: (expired=%v, err=%v)", expired, err)
	}
	if _, err := effects.ExpireUnresolved(context.Background(), b, effects.Claim{}, ttl); err == nil {
		t.Fatal("invalid claim must be refused")
	}
}

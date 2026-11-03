package mutation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

func TestLabelReplayKeepsUnknownAndFencesExpiredObserver(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "ambiguous replay", true: "expired observer"}[expired], func(t *testing.T) {
			b := testBoundary(t, "enforce")
			now := time.Unix(100, 0)
			b.Executor.Now = func() time.Time { return now }
			present := true
			claim := effects.Claim{Repo: "o/r", Kind: effects.KindLabelMutation, Target: "63", Inputs: map[string]string{"remove_label": "claimed"}}
			claim.ObserveApplied = func(context.Context) (effects.ExternalState, error) {
				if expired {
					now = now.Add(time.Hour)
				}
				return effects.ExternalState{Applied: !present}, nil
			}
			calls := 0
			boom := errors.New("timeout")
			effect := func(context.Context) (effects.Result, error) {
				calls++
				if calls > 1 {
					return effects.Result{}, boom
				}
				present = false
				return effects.Result{}, nil
			}
			if _, err := b.Execute(context.Background(), claim, effect); err != nil {
				t.Fatal(err)
			}
			applied, _ := b.Executor.Journal.Get(boundaryEffect(claim).LogicalID())
			present = true
			_, err := b.Execute(context.Background(), claim, effect)
			op, _ := b.Executor.Journal.Get(applied.LogicalID)
			if expired {
				if !errors.Is(err, effects.ErrDenied) || calls != 1 || op.Status != StatusApplied {
					t.Fatalf("expired: err=%v calls=%d status=%s", err, calls, op.Status)
				}
			} else {
				if !errors.Is(err, boom) || calls != 2 || op.Status != StatusUnknown || len(op.Attempts) != 2 {
					t.Fatalf("replay: err=%v calls=%d op=%+v", err, calls, op)
				}
				if err := b.Executor.Journal.reopenLabel(applied, now); !errors.Is(err, ErrResultRegression) {
					t.Fatalf("stale observation accepted: %v", err)
				}
				_, err = b.Execute(context.Background(), claim, effect)
				if !errors.Is(err, effects.ErrNeedsReconciliation) || calls != 2 {
					t.Fatalf("unknown replay: err=%v calls=%d", err, calls)
				}
			}
		})
	}
}

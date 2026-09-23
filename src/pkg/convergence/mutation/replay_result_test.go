package mutation

import (
	"context"
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
)

// Reproduction for #8347: a dedup replay of an already-applied logical
// operation must hand the caller the SAME typed result the original attempt
// produced, not an empty Operation that only says "already done".

const (
	replayPRProvenance = "https://github.com/acme/widgets/pull/101"
	replayEffectCalls  = 1
)

func TestExecutorReplayReturnsRecordedTypedResult(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g, err := x.Ledger.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	calls := 0
	first, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		calls++
		return replayPRProvenance, nil
	})
	if err != nil || first.Status != StatusApplied || first.Result != replayPRProvenance {
		t.Fatalf("first execute = (%+v, %v)", first, err)
	}

	replay, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		calls++
		return "must-not-run", nil
	})
	if !errors.Is(err, ErrAlreadyApplied) {
		t.Fatalf("replay must still be named by ErrAlreadyApplied, got %v", err)
	}
	if calls != replayEffectCalls {
		t.Fatalf("effect ran %d times on replay, want %d", calls, replayEffectCalls)
	}
	// The typed result is the point of the issue: status and provenance must
	// match the original attempt byte for byte.
	if replay.Status != first.Status || replay.Result != first.Result || replay.LogicalID != first.LogicalID {
		t.Fatalf("replay typed result = %+v, want the recorded %+v", replay, first)
	}
}

func TestExecutorUnknownAttemptReplaysAsReconciliationNotEmptySuccess(t *testing.T) {
	x, c, now := newExecutor(t, proof.ModeEnforce)
	g, err := x.Ledger.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	boom := errors.New("api timeout")
	if _, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		return "", boom
	}); !errors.Is(err, boom) {
		t.Fatalf("first execute must surface the effect error, got %v", err)
	}
	replay, err := x.Execute(testEffect(), g.Epoch, "alice", func() (string, error) {
		t.Fatal("effect must not run while the operation is Unknown")
		return "", nil
	})
	if !errors.Is(err, ErrNeedsReconciliation) {
		t.Fatalf("unknown replay = %v, want ErrNeedsReconciliation", err)
	}
	if replay.Status == StatusApplied || replay.Result != "" {
		t.Fatalf("unknown replay must never look like an applied success: %+v", replay)
	}
}

func TestBoundaryReplayReturnsRecordedProvenance(t *testing.T) {
	b := testBoundary(t, proof.ModeEnforce)
	claim := effects.Claim{Repo: "acme/widgets", Kind: effects.KindPullRequestCreate, Target: "feature-1", Actor: "alice"}
	calls := 0
	first, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		calls++
		return effects.Result{Provenance: replayPRProvenance}, nil
	})
	if err != nil || first.Provenance != replayPRProvenance {
		t.Fatalf("first execute = (%+v, %v)", first, err)
	}
	replay, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		calls++
		return effects.Result{Provenance: "must-not-run"}, nil
	})
	if err != nil {
		t.Fatalf("replay must be idempotent success, got %v", err)
	}
	if calls != replayEffectCalls {
		t.Fatalf("effect ran %d times on replay, want %d", calls, replayEffectCalls)
	}
	if replay != first {
		t.Fatalf("replay result = %+v, want the recorded %+v", replay, first)
	}
}

func TestBoundaryUnknownAttemptDoesNotReplayAsSuccess(t *testing.T) {
	b := testBoundary(t, proof.ModeEnforce)
	claim := effects.Claim{Repo: "acme/widgets", Kind: effects.KindPullRequestCreate, Target: "feature-1", Actor: "alice"}
	boom := errors.New("api timeout")
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("first execute must surface the effect error, got %v", err)
	}
	replay, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		t.Fatal("effect must not run while the operation is Unknown")
		return effects.Result{}, nil
	})
	if !errors.Is(err, ErrNeedsReconciliation) || replay.Provenance != "" {
		t.Fatalf("unknown replay = (%+v, %v), want ErrNeedsReconciliation with empty provenance", replay, err)
	}
}

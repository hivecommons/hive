package effects

import (
	"context"
	"errors"
	"testing"
)

type testJournaledBoundary struct {
	calls int
}

func (b *testJournaledBoundary) Execute(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	return effect(ctx)
}

func (b *testJournaledBoundary) ExecuteJournaled(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	b.calls++
	return effect(ctx)
}

func TestLoggingBoundaryJournalRequiredDelegation(t *testing.T) {
	for _, next := range []Boundary{nil, NoopBoundary{}} {
		boundary := LoggingBoundary{Next: next}
		_, err := boundary.ExecuteJournaled(context.Background(), Claim{}, func(context.Context) (Result, error) {
			t.Fatal("unjournaled effect ran")
			return Result{}, nil
		})
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("missing journal was not denied: %v", err)
		}
	}
	next := new(testJournaledBoundary)
	boundary := LoggingBoundary{Next: LoggingBoundary{Next: next}}
	out, err := boundary.ExecuteJournaled(context.Background(), Claim{}, func(context.Context) (Result, error) {
		return Result{Provenance: "exact-subject"}, nil
	})
	if err != nil || out.Provenance != "exact-subject" || next.calls != 1 {
		t.Fatalf("journal-required delegation: result=%+v err=%v calls=%d", out, err, next.calls)
	}
}

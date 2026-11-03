package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The park re-check keeps its own throttle: the hard-suppress reconcile
// running (or being switched off) neither spends nor blocks its pass.
func TestParkRecheckHasItsOwnGate(t *testing.T) {
	if parkRecheckSched == hardSuppressReconcileSched {
		t.Fatal("park re-check shares the hard-suppress gate")
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	gate := &hardSuppressReconcileGate{now: func() time.Time { return now }}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// No client: nothing runs and the gate is not spent.
	runParkRecheckWith(context.Background(), nil, nil, logger, gate)
	if !gate.due(30 * time.Minute) {
		t.Fatal("a skipped pass spent the gate")
	}
}

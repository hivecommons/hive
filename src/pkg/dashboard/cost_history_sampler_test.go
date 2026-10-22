package dashboard

import (
	"context"
	"testing"
	"time"
)

// The server-side sampler records a cost snapshot without any browser
// polling, and stops when its context is cancelled.
func TestCostHistorySamplerSamplesWithoutViewers(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runCostHistorySampler(ctx, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.CostHistory()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(s.CostHistory()); n != 1 {
		t.Fatalf("cost history = %d entries, want 1 from the initial sample", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not stop on context cancellation")
	}
}

// A cancelled context records nothing.
func TestCostHistorySamplerHonoursCancelledContext(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.sampleHistories(ctx)
	if n := len(s.CostHistory()); n != 0 {
		t.Fatalf("cost history = %d entries after a cancelled sample, want 0", n)
	}
}

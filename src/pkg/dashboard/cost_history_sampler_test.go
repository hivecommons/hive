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
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(5 * time.Second)
	for len(s.CostHistory()) == 0 {
		select {
		case <-tick.C:
		case <-timeout:
			t.Fatal("sampler did not record an initial sample")
		}
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

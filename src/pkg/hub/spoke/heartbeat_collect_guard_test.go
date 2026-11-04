package spoke

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNonOverlappingHeartbeatCollector_TimeoutRetryAndRecovery(t *testing.T) {
	ResetHeartbeatStateForTest()
	t.Cleanup(ResetHeartbeatStateForTest)
	var calls atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	collect := nonOverlappingHeartbeatCollector(func() *HeartbeatPayload {
		calls.Add(1)
		<-release
		return &HeartbeatPayload{HiveID: "recovered"}
	})

	if got := collectWithTimeout(context.Background(), collect, 20*time.Millisecond, nil2Logger()); got != nil {
		t.Fatalf("blocked collect returned %+v", got)
	}
	// Repeated beats must be able to fall back without queuing more work on
	// the stuck dependency. Without the guard each call starts another copy.
	for i := 0; i < 5; i++ {
		if got := collectWithTimeout(context.Background(), collect, 20*time.Millisecond, nil2Logger()); got != nil {
			t.Fatalf("busy collect returned %+v", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("started %d collectors while blocked, want 1", got)
	}

	unblock()
	// A late full payload still becomes last-good. Seeing the cache also
	// proves the guard's lock was released before attempting a fresh collect.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for loadLastGoodPayload() == nil {
		select {
		case <-deadline.C:
			t.Fatal("late completion did not populate the fallback cache")
		case <-tick.C:
		}
	}
	if got := collectWithTimeout(context.Background(), collect, time.Second, nil2Logger()); got == nil || got.HiveID != "recovered" {
		t.Fatalf("collect did not recover after blocker cleared: %+v", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("collector calls after recovery = %d, want 2", got)
	}
}

func TestNonOverlappingHeartbeatCollector_PanicReleasesGuard(t *testing.T) {
	calls := 0
	collect := nonOverlappingHeartbeatCollector(func() *HeartbeatPayload {
		calls++
		if calls == 1 {
			panic("collector failed")
		}
		return &HeartbeatPayload{HiveID: "recovered"}
	})
	if got := collectWithTimeout(context.Background(), collect, time.Second, nil2Logger()); got != nil {
		t.Fatalf("panicking collect returned %+v", got)
	}
	if got := collectWithTimeout(context.Background(), collect, time.Second, nil2Logger()); got == nil {
		t.Fatal("panic left collector permanently busy")
	}
}

func TestNonOverlappingHeartbeatCollector_FreshStatsStayBounded(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	finished := make(chan struct{})
	var finishOnce sync.Once
	collect := FreshStatusCollector(nonOverlappingHeartbeatCollector(func() *HeartbeatPayload {
		calls.Add(1)
		<-release
		defer finishOnce.Do(func() { close(finished) })
		return nil
	}))
	t.Cleanup(func() { close(release); <-finished })
	for i := 0; i < 5; i++ {
		if got := collectFreshStatsWithTimeout(context.Background(), collect, 20*time.Millisecond, nil2Logger()); got != nil {
			t.Fatalf("blocked fresh-stats collect returned %+v", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("started %d fresh-stats collectors while blocked, want 1", got)
	}
}

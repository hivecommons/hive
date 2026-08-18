package hooks

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestRateLimiterInPlaceFilterKeepsCorrectTimestamps guards the recent[:0]
// aliasing in allow(): the filter reuses the backing array, so an off-by-one
// there would silently retain or drop the wrong firings and corrupt the
// ceiling in one direction or the other.
func TestRateLimiterInPlaceFilterKeepsCorrectTimestamps(t *testing.T) {
	rl := newRateLimiter()
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

	// Fill the window with 5 firings spread over 50 seconds.
	for i := 0; i < 5; i++ {
		if !rl.allow("h", 5, base.Add(time.Duration(i*10)*time.Second)) {
			t.Fatalf("setup firing %d should be allowed", i)
		}
	}
	if rl.allow("h", 5, base.Add(51*time.Second)) {
		t.Fatal("6th firing inside the window must be refused")
	}

	// At base+65s the cutoff is base+5s, so only the t=0s firing has aged out;
	// t=10,20,30,40 remain (4 of 5). Exactly ONE more slot is free, and the
	// next firing after that must be refused — partial expiry frees exactly
	// the number of slots that actually aged out, never more.
	if !rl.allow("h", 5, base.Add(65*time.Second)) {
		t.Error("the one slot freed by partial expiry should be usable")
	}
	if rl.allow("h", 5, base.Add(65*time.Second)) {
		t.Error("ceiling must still bind after partial expiry")
	}

	// At base+95s the cutoff is base+35s: t=10,20,30 have now aged out too,
	// leaving t=40 and the t=65 firing. Three slots free.
	for i := 0; i < 3; i++ {
		if !rl.allow("h", 5, base.Add(95*time.Second)) {
			t.Errorf("slot %d freed by further expiry should be usable", i)
		}
	}
	if rl.allow("h", 5, base.Add(95*time.Second)) {
		t.Error("ceiling must bind again once the window refills")
	}

	// Retained timestamps must all be inside the window.
	cutoff := base.Add(65 * time.Second).Add(-rateLimitWindow)
	for _, ts := range rl.firings["h"] {
		if !ts.After(cutoff) {
			t.Errorf("stale timestamp %v retained past cutoff %v", ts, cutoff)
		}
	}
}

// TestConcurrentFireIsRaceFree drives many transitions through one dispatcher
// from many goroutines, exercising the registry snapshot + limiter under -race.
func TestConcurrentFireIsRaceFree(t *testing.T) {
	notifier := &fakeNotifier{}
	audit := &fakeAudit{}
	d := NewDispatcher(
		mustRegistry(t,
			Hook{Name: "a", On: TransitionReviewRejected, Action: ActionNotify, RateLimitPerMinute: 100},
			Hook{Name: "b", On: TransitionReviewRejected, Action: ActionNotify, RateLimitPerMinute: 100},
		),
		quietLogger(), WithNotifier(notifier), WithAuditSink(audit))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Fire(context.Background(), Payload{
				Transition: TransitionReviewRejected, Agent: "reviewer",
			})
		}()
	}
	// Concurrent hot reloads race against dispatch.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.SetRegistry(mustRegistry(t, Hook{
				Name: "a", On: TransitionReviewRejected, Action: ActionNotify, RateLimitPerMinute: 100,
			}))
		}()
	}
	wg.Wait()
	d.Wait()

	// The ceiling must have held: never more than the two hooks' combined limit.
	if got := notifier.count(); got > 200 {
		t.Errorf("rate ceiling breached under concurrency: %d notifications", got)
	}
}

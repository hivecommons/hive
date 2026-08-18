package hooks

import (
	"context"
	"strings"
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

// TestRateLimiterWindowIsHalfOpen pins the boundary semantics. The window is
// half-open: a timestamp exactly rateLimitWindow old has aged out (the check is
// After(cutoff), not !Before). So a burst at t=0 plus one firing at exactly
// t=60s means limit+1 firings can occur within a single 60-second SPAN.
//
// That is inherent to a sliding window and is intentional — the guarantee is
// "at most `limit` in any trailing window", not "at most `limit` per calendar
// minute". It is pinned here so the boundary is not later "fixed" into an
// off-by-one that makes capacity fail to recover.
func TestRateLimiterWindowIsHalfOpen(t *testing.T) {
	rl := newRateLimiter()
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	const limit = 3

	for i := 0; i < limit; i++ {
		if !rl.allow("h", limit, base) {
			t.Fatalf("burst firing %d should be allowed", i)
		}
	}
	if rl.allow("h", limit, base) {
		t.Fatal("a firing over the ceiling at the same instant must be refused")
	}

	// At exactly base+window the earlier firings have aged out.
	if !rl.allow("h", limit, base.Add(rateLimitWindow)) {
		t.Error("a firing exactly one window later must be allowed (half-open window)")
	}

	// One nanosecond BEFORE the boundary they have not.
	rl2 := newRateLimiter()
	for i := 0; i < limit; i++ {
		rl2.allow("h", limit, base)
	}
	if rl2.allow("h", limit, base.Add(rateLimitWindow-time.Nanosecond)) {
		t.Error("just inside the window the ceiling must still bind")
	}
}

// TestRateLimiterRecoversAfterLimitShrink: an operator lowering the limit on
// reload must not wedge a hook permanently — capacity has to return once the
// existing firings age out.
func TestRateLimiterRecoversAfterLimitShrink(t *testing.T) {
	rl := newRateLimiter()
	base := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		rl.allow("h", 10, base.Add(time.Duration(i)*time.Second))
	}
	// Reload drops the ceiling to 2 while 10 firings are still in the window.
	if rl.allow("h", 2, base.Add(11*time.Second)) {
		t.Error("a shrunken ceiling must bind immediately")
	}
	// Once they age out, the hook fires again rather than staying wedged.
	if !rl.allow("h", 2, base.Add(70*time.Second)) {
		t.Error("capacity must recover after the window clears; the hook must not wedge")
	}
}

// TestFireCopiesAttrsSoCallerMayReuseTheMap: dispatch is asynchronous, so an
// emitter that mutates or reuses its attrs map after Fire returns — which looks
// completely safe, since Fire appears to have finished — would otherwise race
// the hook goroutines reading it. Run under -race, this fails if Fire ever
// stops copying.
func TestFireCopiesAttrsSoCallerMayReuseTheMap(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t,
			Hook{Name: "a", On: TransitionReviewRejected, Action: ActionNotify,
				When: `attr(t.attrs, "pr") != ""`},
			Hook{Name: "b", On: TransitionReviewRejected, Action: ActionNotify,
				When: `attr(t.attrs, "pr") != ""`},
		),
		quietLogger(), WithNotifier(notifier))

	attrs := map[string]string{AttrPR: "4001"}
	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "reviewer", Attrs: attrs,
	})

	// The caller carries on using its own map, as an emitter reasonably might.
	for i := 0; i < 200; i++ {
		attrs["scratch"] = "mutated"
		delete(attrs, "scratch")
	}
	d.Wait()

	if notifier.count() != 2 {
		t.Errorf("both hooks should have fired against the snapshot, got %d", notifier.count())
	}
}

// TestRateLimitIsConsumedBeforeThePredicate pins a deliberate but surprising
// ordering: the limiter is checked before `when:` is evaluated, so a firing the
// predicate later declines still costs a slot. That keeps a flapping transition
// from becoming a CEL-evaluation storm, at the cost that a highly selective
// predicate on a noisy transition can exhaust quota without ever firing.
//
// Pinned because the opposite order looks like an obvious "optimization" and
// would quietly remove the bound this design relies on.
func TestRateLimitIsConsumedBeforeThePredicate(t *testing.T) {
	notifier := &fakeNotifier{}
	audit := &fakeAudit{}
	now := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)

	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "selective", On: TransitionGovernorModeChange, Action: ActionNotify,
			When:               `t.to == "surge"`,
			RateLimitPerMinute: 2,
		}),
		quietLogger(), WithNotifier(notifier), WithAuditSink(audit),
		WithClock(func() time.Time { return now }),
	)

	// Two non-matching transitions consume the whole quota.
	for i := 0; i < 2; i++ {
		d.Fire(context.Background(), Payload{
			Transition: TransitionGovernorModeChange, To: "idle",
		})
	}
	d.Wait()
	if notifier.count() != 0 {
		t.Fatalf("non-matching predicate must not notify, got %d", notifier.count())
	}

	// A matching transition now finds the quota already spent.
	d.Fire(context.Background(), Payload{
		Transition: TransitionGovernorModeChange, To: "surge",
	})
	d.Wait()

	if notifier.count() != 0 {
		t.Error("quota is consumed before the predicate; the matching firing should be suppressed")
	}
	if got := len(audit.withAction(AuditHookRateLimited)); got != 1 {
		t.Errorf("the suppression must be audited, not silent; got %d entries", got)
	}
}

// hangingNotifier blocks until released, modelling a wedged ntfy endpoint.
type hangingNotifier struct{ release chan struct{} }

func (h hangingNotifier) Send(title, message, priority string) { <-h.release }

// TestHungNotifierTimesOutAndDoesNotWedgeWait: Notifier.Send takes no context,
// so without an explicit bound the action timeout would be decorative for the
// one action shipped end-to-end — a wedged notifier would pin its goroutine
// and Wait() would never return. The hook must instead fail with a timeout.
func TestHungNotifierTimesOutAndDoesNotWedgeWait(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the detached goroutine finish and exit

	audit := &fakeAudit{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "hangs", On: TransitionReviewRejected, Action: ActionNotify}),
		quietLogger(),
		WithNotifier(hangingNotifier{release: release}),
		WithAuditSink(audit),
	)

	// A context already past its deadline stands in for the 30s budget so the
	// test does not have to wait it out.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	d.Fire(ctx, Payload{Transition: TransitionReviewRejected, Agent: "reviewer"})

	settled := make(chan struct{})
	go func() { d.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait() never returned: a hung notifier wedged the dispatcher")
	}

	failures := audit.withAction(AuditHookFailed)
	if len(failures) != 1 {
		t.Fatalf("expected the timeout to be audited as a failure, got %d entries", len(failures))
	}
	if msg, _ := failures[0].fields["error"].(string); !strings.Contains(msg, "timed out") {
		t.Errorf("expected a timeout error, got %q", msg)
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

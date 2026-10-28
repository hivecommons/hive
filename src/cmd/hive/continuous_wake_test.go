package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestContinuousWakeDecision(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 45, 0, 0, time.UTC)
	cases := []struct {
		name      string
		next      time.Time
		lastWake  time.Time
		holdUntil time.Time
		wantDelay time.Duration
		wantDue   bool
	}{
		{name: "nothing scheduled polls", wantDelay: continuousWakePoll},
		{name: "far future capped at poll", next: now.Add(10 * time.Minute), wantDelay: continuousWakePoll},
		{name: "near future sleeps exactly", next: now.Add(12 * time.Second), wantDelay: 12 * time.Second},
		{name: "imminent never below min delay", next: now.Add(200 * time.Millisecond), wantDelay: continuousWakeMinDelay},
		{name: "due now", next: now.Add(-time.Second), wantDue: true},
		{name: "due but inside min gap waits the remainder", next: now.Add(-time.Second), lastWake: now.Add(-10 * time.Second), wantDelay: continuousWakeMinGap - 10*time.Second},
		{name: "due past min gap", next: now.Add(-time.Second), lastWake: now.Add(-continuousWakeMinGap), wantDue: true},
		{name: "due but held re-checks at poll cadence", next: now.Add(-time.Minute), holdUntil: now.Add(5 * time.Minute), wantDelay: continuousWakePoll},
		{name: "due with hold about to lift sleeps the remainder", next: now.Add(-time.Minute), holdUntil: now.Add(7 * time.Second), wantDelay: 7 * time.Second},
		{name: "due once hold has lifted", next: now.Add(-time.Minute), holdUntil: now.Add(-time.Second), wantDue: true},
		{name: "hold does not delay a future re-kick", next: now.Add(10 * time.Second), holdUntil: now.Add(5 * time.Minute), wantDelay: 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, due := continuousWakeDecision(tc.next, now, tc.lastWake, tc.holdUntil)
			if due != tc.wantDue || delay != tc.wantDelay {
				t.Fatalf("got (%v, %v), want (%v, %v)", delay, due, tc.wantDelay, tc.wantDue)
			}
		})
	}
}

// TestRunLoopContinuousWakeRunsEarlyEval pins the fix for the reviewer that
// sat idle at its prompt after "kick turn ended": with a continuous agent
// whose NextKick has elapsed, firing the wake timer must run an eval cycle
// without waiting for the cadence ticker, log one line naming the agent, and
// re-arm; firing it before the NextKick elapses must only re-arm.
func TestRunLoopContinuousWakeRunsEarlyEval(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 900, 0)
	b.cfg.Governor.Modes = map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"alpha": "continuous"}}}
	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	b.gov.Evaluate(0, 0, 0, 0) // arms the initial continuous kick 1m out

	gov := newFakeTicker()
	wake := newFakeTicker()
	var armed []time.Duration
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	deps := h.deps()
	deps.newWakeTimer = func(d time.Duration) loopTicker { armed = append(armed, d); return wake }
	done := runLoopAsync(b, deps)
	<-h.persisted // first cycle done

	// Not due yet: the wake only re-arms, no eval. The unbuffered fire
	// rendezvous with the select, which the loop reaches only after arming.
	wake.fire()
	if len(armed) != 1 || armed[0] <= 0 || armed[0] > continuousWakePoll {
		t.Fatalf("wake armed = %v, want one delay in (0, %v]", armed, continuousWakePoll)
	}
	// Make the re-kick due and fire again: an early eval runs.
	b.gov.SeedContinuousNextKickForTest("alpha", time.Now().Add(-time.Second))
	wake.fire()
	<-h.persisted
	cancel()
	waitDone(t, done)

	if len(h.evals) != 2 {
		t.Fatalf("evals = %v, want first cycle + one wake-driven eval", h.evals)
	}
	if !strings.Contains(log.String(), "continuous re-kick wake: running an early eval cycle") || !strings.Contains(log.String(), "agent=alpha") {
		t.Fatalf("missing wake log line:\n%s", log.String())
	}
	if len(wake.resets) < 1 {
		t.Fatalf("wake must re-arm after every fire, resets=%v", wake.resets)
	}
}

func TestContinuousWakeHold(t *testing.T) {
	now := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	due := now.Add(-time.Minute)
	var h continuousWakeHold

	if _, stalled := h.observe(time.Time{}, time.Time{}, "", "", now); stalled {
		t.Fatal("nothing scheduled must not stall")
	}
	if _, stalled := h.observe(due, due.Add(15*time.Minute), "alpha", "alpha", now); stalled {
		t.Fatal("a rescheduled re-kick must not stall")
	}
	if !h.untilFor(due, "alpha").IsZero() {
		t.Fatal("no hold expected before a stall")
	}

	backoff, stalled := h.observe(due, due, "alpha", "alpha", now)
	if !stalled || backoff != continuousWakeStallBackoff {
		t.Fatalf("first stall = (%v, %v), want (%v, true)", backoff, stalled, continuousWakeStallBackoff)
	}
	if got := h.untilFor(due, "alpha"); !got.Equal(now.Add(continuousWakeStallBackoff)) {
		t.Fatalf("untilFor = %v, want %v", got, now.Add(continuousWakeStallBackoff))
	}
	if !h.untilFor(due, "beta").IsZero() || !h.untilFor(due.Add(time.Second), "alpha").IsZero() {
		t.Fatal("hold must be keyed on the exact (NextKick, agent) pair")
	}

	// Each further stall on the same re-kick doubles, capped.
	want := continuousWakeStallBackoff
	for i := 0; i < 8; i++ {
		want *= 2
		if want > continuousWakeStallMax {
			want = continuousWakeStallMax
		}
		backoff, stalled = h.observe(due, due, "alpha", "alpha", now)
		if !stalled || backoff != want {
			t.Fatalf("stall %d = (%v, %v), want (%v, true)", i+2, backoff, stalled, want)
		}
	}

	// The schedule moving releases the hold entirely.
	if _, stalled = h.observe(due, time.Time{}, "alpha", "", now); stalled {
		t.Fatal("a consumed re-kick must release the hold")
	}
	if !h.untilFor(due, "alpha").IsZero() {
		t.Fatal("hold must be cleared once the re-kick moved")
	}
}

// TestRunLoopContinuousWakeBacksOffWhenDueAgentIsSkipped covers the due
// continuous agent that runEvalCycle drops after the governor said "kick"
// (dashboard pause, first-scan deferral, provider-budget gate, upgrade
// drain): its NextKick stays in the past, so without a hold the wake would
// run a full eval cycle every continuousWakeMinGap. The fake eval never
// touches the governor, which is exactly that shape. Evals must stop growing
// while the hold is in force, and the hold must double on a repeat stall.
func TestRunLoopContinuousWakeBacksOffWhenDueAgentIsSkipped(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 900, 0)
	b.cfg.Governor.Modes = map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"alpha": "continuous"}}}
	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	b.gov.Evaluate(0, 0, 0, 0)

	var mu sync.Mutex
	clock := time.Now()
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	gov := newFakeTicker()
	wake := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	deps := h.deps()
	deps.newWakeTimer = func(time.Duration) loopTicker { return wake }
	deps.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	done := runLoopAsync(b, deps)
	<-h.persisted // first cycle

	// Due, skipped: one wake-driven eval, then the hold starts.
	b.gov.SeedContinuousNextKickForTest("alpha", clock.Add(-time.Second))
	wake.fire()
	<-h.persisted
	if len(h.evals) != 2 {
		t.Fatalf("evals = %d after first wake, want 2", len(h.evals))
	}

	// Past the min gap but inside the hold: the wake must only re-arm. A
	// fire rendezvous with the select only after the previous fire was
	// handled, so the second fire proves the first ran without an eval
	// (and that the stall line, logged after persist, has been written).
	advance(continuousWakeMinGap + time.Second)
	wake.fire()
	wake.fire()
	if len(h.evals) != 2 {
		t.Fatalf("evals = %d inside the hold, want still 2", len(h.evals))
	}
	if !strings.Contains(log.String(), "left the due re-kick unchanged") {
		t.Fatalf("missing stall log line:\n%s", log.String())
	}

	// Hold lifted, still skipped: one more eval and the hold doubles.
	advance(continuousWakeStallBackoff)
	wake.fire()
	<-h.persisted
	if len(h.evals) != 3 {
		t.Fatalf("evals = %d after the hold lifted, want 3", len(h.evals))
	}
	advance(continuousWakeStallBackoff + continuousWakeMinGap)
	wake.fire()
	wake.fire()
	if len(h.evals) != 3 {
		t.Fatalf("evals = %d inside the doubled hold, want still 3", len(h.evals))
	}
	if !strings.Contains(log.String(), "backoff="+(2*continuousWakeStallBackoff).String()) {
		t.Fatalf("missing doubled back-off in log:\n%s", log.String())
	}

	// The schedule moving (the agent got kicked) releases the hold: a wake
	// on the new due time evals immediately.
	b.gov.SeedContinuousNextKickForTest("alpha", clock.Add(-time.Millisecond))
	advance(continuousWakeMinGap)
	wake.fire()
	<-h.persisted
	cancel()
	waitDone(t, done)
	if len(h.evals) != 4 {
		t.Fatalf("evals = %d after the re-kick moved, want 4", len(h.evals))
	}
}

// A long cadence cycle leaves another cadence tick ready and a continuous
// re-kick due. No wake timer is fired: the loop must serve the re-kick first.
func TestRunLoopContinuousWakeServedBetweenBusyTicks(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 900, 0)
	defer cancel()
	b.cfg.Governor.Modes = map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"alpha": "continuous"}}}
	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	b.gov.Evaluate(0, 0, 0, 0)
	clock := time.Now()
	b.gov.SeedContinuousNextKickForTest("alpha", clock.Add(time.Minute))
	gov := &fakeTicker{ch: make(chan time.Time, 1)}
	wake := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	deps := h.deps()
	deps.now = func() time.Time { return clock }
	deps.newWakeTimer = func(time.Duration) loopTicker { return wake }
	eval := deps.runEval
	deps.runEval = func(b *boot, restarted []string, source evalCycleSource) {
		eval(b, restarted, source)
		switch len(h.evals) {
		case 2:
			clock = clock.Add(10 * time.Minute)
			gov.ch <- clock // cadence elapsed again while the cycle was busy
		case 3:
			// Stop after the competing next cycle, whichever source won.
			select {
			case <-gov.ch:
			default:
			}
			cancel()
		}
	}
	done := runLoopAsync(b, deps)
	<-h.persisted
	gov.fire()
	waitDone(t, done)
	if len(h.sources) != 3 || h.sources[2] != evalSourceCached {
		t.Fatalf("sources = %v, want initial/cadence followed by cached re-kick", h.sources)
	}
	if h.sweeps != 2 || h.rotations != 2 {
		t.Fatalf("sweeps=%d rotations=%d: continuous wake must omit maintenance", h.sweeps, h.rotations)
	}
	if !strings.Contains(log.String(), "continuous re-kick wake: running an early eval cycle") {
		t.Fatalf("missing continuous wake: %s", log.String())
	}
}

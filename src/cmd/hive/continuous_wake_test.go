package main

import (
	"strings"
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, due := continuousWakeDecision(tc.next, now, tc.lastWake)
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

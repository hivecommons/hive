package watchdog

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// An agent that stays dead under observe mode reaches the same would-restart
// verdict on every sweep, because observe mode takes no action. The hive in
// hivecommons/hive#19393 logged that as 79 WARNs in 10m for one agent and
// self-reported it fleet-wide; one unchanged verdict must be reported once per
// observeDecisionRelogInterval, and afresh after the agent recovers.
func TestObserveModeWouldRestartReportedOncePerWindow(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("strategist")
	fleet.setObs("strategist", deadObs())
	alerter := newFakeAlerter()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := fastSettings()
	s.Mode = ModeObserve
	r := New(s, fleet, alerter, logger, WithClock(clock.Now))

	const warnLine = `msg="watchdog: would restart dead agent (observe mode; no action taken)"`
	for i := 0; i < 20; i++ {
		r.Tick(context.Background())
		clock.Advance(10 * time.Second)
	}
	if fleet.restartCount() != 0 {
		t.Fatalf("observe mode must not restart, got %d", fleet.restartCount())
	}
	if got := buf.count(warnLine); got != 1 {
		t.Fatalf("20 sweeps of one dead agent logged %d would-restart WARNs, want 1", got)
	}
	if got := len(alerter.auditsFor(auditActionRestart + observedSuffix)); got != 1 {
		t.Fatalf("20 sweeps of one dead agent wrote %d would-restart audits, want 1", got)
	}

	// Past the window the unchanged verdict is reported again.
	clock.Advance(observeDecisionRelogInterval)
	r.Tick(context.Background())
	if got := buf.count(warnLine); got != 2 {
		t.Fatalf("after the relog window: %d WARNs, want 2", got)
	}

	// Recovery clears the record: the next death is news.
	fleet.setObs("strategist", readyObs())
	r.Tick(context.Background())
	fleet.setObs("strategist", deadObs())
	r.Tick(context.Background())
	if got := buf.count(warnLine); got != 3 {
		t.Fatalf("a death after recovery: %d WARNs, want 3", got)
	}
}

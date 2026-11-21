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

// The relog window is half-open: a repeat strictly inside it is suppressed,
// and one exactly observeDecisionRelogInterval later is reported again.
func TestShouldReportObserveDecisionWindowBoundary(t *testing.T) {
	rec := &agentRecord{}
	t0 := time.Unix(1_700_000_000, 0)
	if !shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0) {
		t.Fatal("first decision must be reported")
	}
	if shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0.Add(observeDecisionRelogInterval-time.Nanosecond)) {
		t.Fatal("a repeat just inside the window must not be reported")
	}
	if !shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0.Add(observeDecisionRelogInterval)) {
		t.Fatal("a repeat exactly one interval later must be reported")
	}
	if !shouldReportObserveDecisionLocked(rec, auditActionPause, ClassStuckOverlay, t0.Add(observeDecisionRelogInterval)) {
		t.Fatal("a different action must be reported at once")
	}
}

// An agent flapping between dead classes without recovering must not defeat
// the limiter: each (action, class) decision has its own window, so flips
// inside the window are not re-reported (review follow-up on #19394).
func TestObserveDecisionFlappingDeadClassesReportedOncePerWindow(t *testing.T) {
	rec := &agentRecord{}
	t0 := time.Unix(1_700_000_000, 0)
	reports := 0
	for i := 0; i < 20; i++ {
		cls := ClassStuckOverlay
		if i%2 == 1 {
			cls = ClassNoOutput
		}
		if shouldReportObserveDecisionLocked(rec, auditActionRestart, cls, t0.Add(time.Duration(i)*10*time.Second)) {
			reports++
		}
	}
	if reports != 2 {
		t.Fatalf("20 sweeps flipping between two dead classes reported %d times, want 2 (one per class)", reports)
	}
}

// Only a ready pane is a recovery. A transient unknown or auth-required
// capture between dead sweeps must not reopen the window; a ready one does.
func TestObserveDecisionResetOnlyOnReady(t *testing.T) {
	rec := &agentRecord{}
	t0 := time.Unix(1_700_000_000, 0)
	if !shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0) {
		t.Fatal("first decision must be reported")
	}
	for _, cls := range []PaneClass{ClassUnknown, ClassAuthRequired, ClassNoOutput} {
		rec.observeDecision.resetOnReady(cls)
		if shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0.Add(time.Second)) {
			t.Fatalf("a %s capture must not reset the observe-decision window", cls)
		}
	}
	rec.observeDecision.resetOnReady(ClassReady)
	if !shouldReportObserveDecisionLocked(rec, auditActionRestart, ClassStuckOverlay, t0.Add(2*time.Second)) {
		t.Fatal("a ready capture must reset the window so the next death is reported")
	}
}

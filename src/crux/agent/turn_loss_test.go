package agent

import (
	"testing"
	"time"
)

// These tests pin the INSTRUMENT, not a behaviour change: nothing here alters
// when hive tears an agent down, only what it records about it (#4002 open
// question 3 — RestartCount says how often, nothing said what it cost).
//
// The property that matters most is that the instrument does not overclaim.
// The RFC is arguing FOR conversation-as-state; a measurement that inflates the
// loss would be evidence manufactured by the side that wants it. So the
// interesting cases below are the ones that must NOT count as lost work.

func turnLossManager(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   discardLogger(),
	}
	proc := &AgentProcess{Name: "scanner"}
	m.agents["scanner"] = proc
	return m, proc
}

// fixedClock pins turnLossNow so durations are exact rather than approximate.
func fixedClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := turnLossNow
	turnLossNow = func() time.Time { return at }
	t.Cleanup(func() { turnLossNow = prev })
}

func TestTearDownRecordsInterruptedTurn(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	kick := now.Add(-5 * time.Minute)
	proc.LastKick = &kick
	proc.LastPaneChange = now.Add(-30 * time.Second) // produced during this turn
	proc.kickLogPending = true

	m.tearDownTurnLocked(proc, "restart")

	if proc.TurnLoss.Interruptions != 1 {
		t.Fatalf("Interruptions = %d, want 1", proc.TurnLoss.Interruptions)
	}
	if proc.TurnLoss.Producing != 1 {
		t.Errorf("Producing = %d, want 1 — the pane changed after the kick landed", proc.TurnLoss.Producing)
	}
	if proc.TurnLoss.UpperBound != 5*time.Minute {
		t.Errorf("UpperBound = %v, want 5m (teardown minus kick delivery)", proc.TurnLoss.UpperBound)
	}
	if proc.kickLogPending {
		t.Error("kickLogPending still set after teardown; the turn would be counted twice")
	}
	rec := proc.TurnLoss.Recent[0]
	if rec.Reason != "restart" {
		t.Errorf("Reason = %q, want restart", rec.Reason)
	}
	if rec.SinceOutput == nil || *rec.SinceOutput != 30*time.Second {
		t.Errorf("SinceOutput = %v, want 30s", rec.SinceOutput)
	}
}

// TestTearDownIgnoresAgentWithNothingPending is the discriminator that keeps
// the number meaningful. A kick is only delivered into a pane sitting at its
// input marker, so a completed-and-rotated turn has nothing pending — counting
// those would drown the signal in finished work.
func TestTearDownIgnoresAgentWithNothingPending(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	kick := now.Add(-5 * time.Minute)
	proc.LastKick = &kick
	proc.kickLogPending = false

	m.tearDownTurnLocked(proc, "restart")

	if proc.TurnLoss.Interruptions != 0 {
		t.Errorf("Interruptions = %d, want 0 for an agent with no pending turn output", proc.TurnLoss.Interruptions)
	}
}

// TestIdleAgentIsRecordedButNotProducing is the overclaim guard. An agent that
// finished its turn and then sat idle still carries pending output, and
// charging that whole idle stretch to the restart as lost work is exactly the
// inflation this instrument must not do. It is still RECORDED — the upper
// bound is real — but Producing must stay false so analysis can discount it.
func TestIdleAgentIsRecordedButNotProducing(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	kick := now.Add(-2 * time.Hour)
	proc.LastKick = &kick
	// Last output predates the kick: nothing happened during this turn.
	proc.LastPaneChange = now.Add(-3 * time.Hour)
	proc.kickLogPending = true

	m.tearDownTurnLocked(proc, "shutdown")

	if proc.TurnLoss.Interruptions != 1 {
		t.Fatalf("Interruptions = %d, want 1", proc.TurnLoss.Interruptions)
	}
	if proc.TurnLoss.Producing != 0 {
		t.Error("Producing = 1 for an agent whose pane never changed after the kick; the instrument is overclaiming")
	}
}

// TestUnknownPaneActivityIsNotProducing: no observed pane change at all reads
// as UNKNOWN. Guessing "yes" would inflate the very number the RFC is trying
// to size, so unknown must fall to false.
func TestUnknownPaneActivityIsNotProducing(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	kick := now.Add(-time.Minute)
	proc.LastKick = &kick
	proc.kickLogPending = true // LastPaneChange left zero — never observed

	m.tearDownTurnLocked(proc, "restart")

	rec := proc.TurnLoss.Recent[0]
	if rec.SinceOutput != nil {
		t.Errorf("SinceOutput = %v, want nil (unknown) when the pane poller saw nothing", rec.SinceOutput)
	}
	if rec.Producing {
		t.Error("Producing = true on unknown pane activity; unknown must not be read as working")
	}
}

// TestNoKickRecordedLeavesUpperBoundZero: an agent torn down with pending
// output but no recorded kick has no clock to measure against. Zero is the
// honest answer; inventing one from process start would be fabrication.
func TestNoKickRecordedLeavesUpperBoundZero(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	proc.kickLogPending = true // LastKick nil

	m.tearDownTurnLocked(proc, "restart")

	if proc.TurnLoss.UpperBound != 0 {
		t.Errorf("UpperBound = %v, want 0 with no recorded kick", proc.TurnLoss.UpperBound)
	}
	if proc.TurnLoss.Interruptions != 1 {
		t.Errorf("Interruptions = %d, want the teardown still counted", proc.TurnLoss.Interruptions)
	}
}

// TestRecentRingIsBounded: the aggregate counters are unbounded and cheap, but
// Recent rides the persisted state file, so it must not grow without limit.
func TestRecentRingIsBounded(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fixedClock(t, now)
	m, proc := turnLossManager(t)

	kick := now.Add(-time.Minute)
	for i := 0; i < turnLossRecentCapacity+15; i++ {
		proc.LastKick = &kick
		proc.kickLogPending = true
		m.tearDownTurnLocked(proc, "restart")
	}

	if got := len(proc.TurnLoss.Recent); got != turnLossRecentCapacity {
		t.Errorf("len(Recent) = %d, want it capped at %d", got, turnLossRecentCapacity)
	}
	// The aggregate must still count every one of them — capping the detail
	// must not cap the measurement.
	if want := turnLossRecentCapacity + 15; proc.TurnLoss.Interruptions != want {
		t.Errorf("Interruptions = %d, want %d — the ring cap must not truncate the total",
			proc.TurnLoss.Interruptions, want)
	}
}

// TestSeedTurnLossSurvivesTheRestartItMeasures. Without seeding, the counter
// resets on exactly the event it exists to count, and every spoke would report
// only what its current process lifetime lost — near zero on a fleet that rolls
// often, which is the wrong answer in the direction that kills the RFC.
func TestSeedTurnLossSurvivesTheRestartItMeasures(t *testing.T) {
	m, proc := turnLossManager(t)

	m.SeedTurnLoss("scanner", TurnLoss{
		Interruptions: 7,
		Producing:     4,
		UpperBound:    12 * time.Minute,
		Bytes:         2048,
		Recent:        []TurnInterruption{{Reason: "restart"}},
	})

	if proc.TurnLoss.Interruptions != 7 || proc.TurnLoss.Producing != 4 {
		t.Errorf("seeded TurnLoss = %+v, want the persisted counters restored", proc.TurnLoss)
	}
	if proc.TurnLoss.UpperBound != 12*time.Minute {
		t.Errorf("UpperBound = %v, want 12m", proc.TurnLoss.UpperBound)
	}
}

// TestSeedTurnLossTrimsAnOversizedRing guards against a hand-edited or
// older-format state file re-introducing unbounded growth through the back door.
func TestSeedTurnLossTrimsAnOversizedRing(t *testing.T) {
	m, proc := turnLossManager(t)

	oversized := make([]TurnInterruption, turnLossRecentCapacity+10)
	m.SeedTurnLoss("scanner", TurnLoss{Interruptions: 1, Recent: oversized})

	if got := len(proc.TurnLoss.Recent); got != turnLossRecentCapacity {
		t.Errorf("len(Recent) = %d, want trimmed to %d", got, turnLossRecentCapacity)
	}
}

// TestCloneTurnLossDoesNotAliasRecent: AllStatuses/snapshot hand this to
// readers outside m.mu, so an aliased slice would race the next teardown's
// append.
func TestCloneTurnLossDoesNotAliasRecent(t *testing.T) {
	orig := TurnLoss{Interruptions: 1, Recent: []TurnInterruption{{Reason: "restart"}}}
	clone := cloneTurnLoss(orig)
	clone.Recent[0].Reason = "mutated"

	if orig.Recent[0].Reason != "restart" {
		t.Error("cloneTurnLoss aliased Recent; a reader could race the next teardown")
	}
}

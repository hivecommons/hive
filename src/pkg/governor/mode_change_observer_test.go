package governor

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests cover the ModeChangeObserver added for RFC #4001's
// governor_mode_change transition. The properties that matter are that the
// observer runs only for a COMMITTED change, and that it runs OUTSIDE g.mu —
// hive has already been bitten by re-entrant-locking startup deadlocks, and an
// observer that reads governor state is the obvious way to reintroduce one.

func observerTestGovernor(t *testing.T) *Governor {
	t.Helper()
	return New(
		config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"quiet": {Threshold: 1},
				"busy":  {Threshold: 5},
				"surge": {Threshold: 10},
			},
		},
		map[string]config.AgentConfig{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// TestModeChangeObserverFiresOnCommittedChange is the positive control: the
// observer must actually be invoked, with the committed from/to.
func TestModeChangeObserverFiresOnCommittedChange(t *testing.T) {
	g := observerTestGovernor(t)

	var mu sync.Mutex
	var seen []ModeChange
	g.SetModeChangeObserver(func(c ModeChange) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, c)
	})

	// Drive the queue deep enough to leave idle.
	g.Evaluate(20, 0, 0, 0)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 observed mode change, got %d", len(seen))
	}
	if seen[0].To == seen[0].From {
		t.Errorf("observed a no-op change: %+v", seen[0])
	}
	if seen[0].To != ModeSurge {
		t.Errorf("expected surge for a deep queue, got %q", seen[0].To)
	}
}

// TestModeChangeObserverSilentWhenModeUnchanged: no transition, no firing.
// Otherwise every eval cycle would emit, and a hook on this transition would
// become a per-cycle notification storm.
func TestModeChangeObserverSilentWhenModeUnchanged(t *testing.T) {
	g := observerTestGovernor(t)

	var mu sync.Mutex
	count := 0
	g.SetModeChangeObserver(func(ModeChange) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	// Same depth twice: one change, then nothing.
	g.Evaluate(20, 0, 0, 0)
	g.Evaluate(20, 0, 0, 0)
	g.Evaluate(20, 0, 0, 0)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 firing across 3 evals at a constant depth, got %d", count)
	}
}

// TestModeChangeObserverRunsOutsideTheGovernorLock is the deadlock regression.
// The observer reads governor state (Mode()), which takes g.mu.RLock(). If the
// observer were invoked while Evaluate still held g.mu, this test would
// deadlock rather than fail — so it is run with a timeout by the test runner
// and documents the invariant explicitly.
func TestModeChangeObserverRunsOutsideTheGovernorLock(t *testing.T) {
	g := observerTestGovernor(t)

	var observedMode Mode
	g.SetModeChangeObserver(func(c ModeChange) {
		// Re-entering the governor from the observer MUST be safe.
		observedMode = g.GetState().Mode
	})

	g.Evaluate(20, 0, 0, 0)

	if observedMode != ModeSurge {
		t.Errorf("observer should see the committed state, got %q", observedMode)
	}
}

// TestModeChangeObserverSeesCommittedState: the observer must run AFTER the
// state flip, not before, so a hook never acts on a transition the governor
// then declines to make.
func TestModeChangeObserverSeesCommittedState(t *testing.T) {
	g := observerTestGovernor(t)

	var stateAtFire Mode
	g.SetModeChangeObserver(func(c ModeChange) {
		stateAtFire = g.GetState().Mode
	})

	g.Evaluate(20, 0, 0, 0)

	if stateAtFire != ModeSurge {
		t.Errorf("observer ran before the commit: governor mode was %q at fire time", stateAtFire)
	}
}

func TestNilModeChangeObserverIsSafe(t *testing.T) {
	g := observerTestGovernor(t)
	// No observer installed: must not panic.
	g.Evaluate(20, 0, 0, 0)

	// Explicitly nil, too.
	g.SetModeChangeObserver(nil)
	g.Evaluate(0, 0, 0, 0)
}

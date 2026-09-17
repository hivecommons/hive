package loginscan

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// TestScanPrunesVanishedAgentsFromTheSightingMap pins the Retain sweep at the
// end of the loop. The tracker is process-scoped and lives for the life of the
// hive, so an agent that is removed from config — or simply stops — must not
// keep its streak alive forever: the map would grow without bound, and worse,
// a name that is later reused would inherit a stale streak and could be paused
// on its FIRST real sighting instead of the second.
func TestScanPrunesVanishedAgentsFromTheSightingMap(t *testing.T) {
	t.Parallel()

	sightings := NewSightingTracker()
	// A previous cycle saw "departed" matching; it is now gone from the fleet.
	sightings.Observe("departed", true)

	mgr := &fakeLoginScanManager{
		statuses: map[string]*agent.AgentProcess{
			"present": {State: agent.StateRunning},
		},
		outputs: map[string][]string{
			"present": {"build passed"},
		},
	}
	cfg := loginScanLoopConfig([]string{"please log in"}, map[string]string{
		"present": "claude",
	})

	Scan(context.Background(), cfg, mgr, &fakeLoginScanNotifier{},
		&fakeLoginScanAuditor{}, testLogger(), sightings)

	// If the sweep ran, "departed" was dropped and its next sighting starts a
	// fresh streak at 1. If it did not, the stale streak makes this 2 — which
	// is PauseMinSightings, i.e. an immediate pause on a single sighting.
	if got := sightings.Observe("departed", true); got != 1 {
		t.Fatalf("vanished agent kept its streak: next observation got %d, want 1", got)
	}
}

// TestScanSkipsInvalidPatternsButKeepsScanning pins that one bad regex in
// governor.sensing.login_patterns is skipped rather than aborting the scan.
// Operator-supplied patterns are free text; if a single typo disabled the
// whole detector, every genuinely logged-out agent would go undetected and the
// only evidence would be one Warn line at startup.
func TestScanSkipsInvalidPatternsButKeepsScanning(t *testing.T) {
	t.Parallel()

	mgr := &fakeLoginScanManager{
		statuses: map[string]*agent.AgentProcess{
			"stuck": {State: agent.StateRunning},
		},
		outputs: map[string][]string{
			"stuck": {"Please log in to continue"},
		},
	}
	notifier := &fakeLoginScanNotifier{}
	auditor := &fakeLoginScanAuditor{}
	sightings := NewSightingTracker()
	sightings.Observe("stuck", true) // already at the streak threshold

	// The invalid pattern is FIRST, so an abort-on-error would never reach the
	// valid one that follows it.
	cfg := loginScanLoopConfig([]string{"[unclosed", "please log in"}, map[string]string{
		"stuck": "claude",
	})

	Scan(context.Background(), cfg, mgr, notifier, auditor, testLogger(), sightings)

	if len(mgr.pauses) != 1 || mgr.pauses[0] != "stuck" {
		t.Fatalf("a bad pattern disabled the detector: pauses = %v, want [stuck]", mgr.pauses)
	}
}

// TestScanWithOnlyInvalidPatternsDoesNothing is the companion: when NO pattern
// compiles there is nothing to match on, and the scan must return without
// touching any agent rather than falling through to an empty match set.
func TestScanWithOnlyInvalidPatternsDoesNothing(t *testing.T) {
	t.Parallel()

	mgr := &fakeLoginScanManager{
		statuses: map[string]*agent.AgentProcess{
			"stuck": {State: agent.StateRunning},
		},
		outputs: map[string][]string{
			"stuck": {"Please log in to continue"},
		},
	}
	cfg := loginScanLoopConfig([]string{"[unclosed", "   "}, map[string]string{
		"stuck": "claude",
	})

	Scan(context.Background(), cfg, mgr, &fakeLoginScanNotifier{},
		&fakeLoginScanAuditor{}, testLogger(), NewSightingTracker())

	if len(mgr.getOutputCalls) != 0 {
		t.Fatalf("scan read panes with no usable patterns: %v", mgr.getOutputCalls)
	}
	if len(mgr.pauses) != 0 {
		t.Fatalf("scan paused with no usable patterns: %v", mgr.pauses)
	}
}

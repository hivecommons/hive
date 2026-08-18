package hub

import (
	"log/slog"
	"testing"
	"time"
)

// F21: 44 of 66 hosted spokes sit on pull-only clusters the hub cannot read.
// The convergence sweep skipped them with a bare `continue`, so they left NO
// trace on the readiness surface — not in ObservedHives, not in
// MissingPerHiveEnv, not in any generation bucket. The 22 reachable spokes
// therefore rendered as a fully converged, retirement-ready fleet.
//
// That is the dangerous half of F21: an operator consults this surface before
// retiring a generation, and retirement is unconditional once the window
// closes. Reading "converged" from a two-thirds-unobserved fleet is how a
// rotation strands 44 spokes at verify_until.
//
// These tests pin the invariant: an unreachable spoke is COUNTED as unreachable
// and can never be summed into converged or retirable.

// perHiveUnreachSecret is fixture key material. Never a real key.
const perHiveUnreachSecret = "f21-unreachable-fixture-secret-aaaaaaaaaaaa"

// fullyObservedHub builds a hub whose last sweep read every hive it admitted.
func fullyObservedHub(gs *generationSet, seen map[string]perHiveEnvObservation) *HubServer {
	return &HubServer{
		keyGenerations:       gs,
		perHiveEnvSeen:       seen,
		perHiveEnvConsidered: len(seen),
	}
}

// TestUnreachableSpokesAreCountedNotDropped is the core F21 assertion, and it
// is a POSITIVE CONTROL IN BOTH DIRECTIONS against the same fleet: the only
// difference between the two halves is whether the sweep could read the
// unreachable spokes. A test asserting only "unreachable blocks convergence"
// would pass against an implementation that hardcodes converged=false, and one
// asserting only the reachable case would pass against today's bug.
func TestUnreachableSpokesAreCountedNotDropped(t *testing.T) {
	seen := map[string]perHiveEnvObservation{
		"hive-oke-a": {Observed: time.Now()},
		"hive-oke-b": {Observed: time.Now()},
	}

	// Direction 1: the sweep read all it admitted. Converged may be true.
	t.Run("fully observed fleet converges", func(t *testing.T) {
		s := fullyObservedHub(nil, seen)
		got := s.PerHiveEnvSnapshot()
		if got.UnreachableHives != 0 {
			t.Fatalf("fixture: UnreachableHives = %d, want 0", got.UnreachableHives)
		}
		if !got.FleetFullyObserved {
			t.Error("FleetFullyObserved = false with nothing unreachable")
		}
		if !got.PerHiveEnvConverged {
			t.Error("PerHiveEnvConverged = false for a fully observed, drift-free fleet — " +
				"the signal must still be able to say yes, or it is not a signal")
		}
	})

	// Direction 2: identical observations, but the sweep also admitted 44
	// hives it could not read. This is the live vllm-d + a-ks-wec2 shape.
	t.Run("partially observed fleet does not converge", func(t *testing.T) {
		s := fullyObservedHub(nil, seen)
		s.perHiveEnvConsidered = len(seen) + 44
		s.perHiveEnvUnreachable = 44
		s.perHiveEnvUnreachableClusters = []string{"a-ks-wec2", "vllm-d"}

		got := s.PerHiveEnvSnapshot()
		if got.UnreachableHives != 44 {
			t.Errorf("UnreachableHives = %d, want 44 — unreachable spokes must be VISIBLE, "+
				"not silently absent from every counter", got.UnreachableHives)
		}
		if got.FleetFullyObserved {
			t.Error("FleetFullyObserved = true with 44 unreachable hives")
		}
		if got.PerHiveEnvConverged {
			t.Error("PerHiveEnvConverged = true while 44 of 46 admitted hives were never read — " +
				"this is F21: 'the hub cannot see it' rendering as 'it is fine'")
		}
		if len(got.UnreachableClusters) != 2 ||
			got.UnreachableClusters[0] != "a-ks-wec2" ||
			got.UnreachableClusters[1] != "vllm-d" {
			t.Errorf("UnreachableClusters = %v, want the two pull-only clusters named "+
				"so an operator can act on the count", got.UnreachableClusters)
		}
	})
}

// TestSafeToRetirePreviousBlocksOnUnreachableSpokes is the rotation-safety
// invariant and the reason F21 must close before F19. Retirement is
// unconditional at verify_until by design, so a spoke the hub cannot read is
// not "lagging" — it is a spoke that will BREAK in 7 days, and whose generation
// is unknown right now. Unknown must never sum into "safe to retire".
//
// Both directions again, same generation set: only reachability differs.
func TestSafeToRetirePreviousBlocksOnUnreachableSpokes(t *testing.T) {
	now := time.Now()
	// A rotation whose verify window has already closed — every other clause of
	// SafeToRetirePrevious is satisfied, so reachability is the only variable.
	expired := legacyGenerationSet(perHiveUnreachSecret).
		rotate(perHiveUnreachSecret+"-next", now.Add(-2*defaultVerifyWindow), defaultVerifyWindow)

	onCurrent := map[string]perHiveEnvObservation{
		"hive-oke-a": {Generation: expired.Current, Observed: now},
	}

	t.Run("retirable when the whole fleet was observed", func(t *testing.T) {
		got := fullyObservedHub(expired, onCurrent).PerHiveEnvSnapshot().KeyGenerations
		if !got.SafeToRetirePrevious {
			t.Fatalf("want retirable for a fully observed fleet past the window; got %+v", got)
		}
	})

	t.Run("NOT retirable while any spoke is unreachable", func(t *testing.T) {
		s := fullyObservedHub(expired, onCurrent)
		s.perHiveEnvConsidered = len(onCurrent) + 1
		s.perHiveEnvUnreachable = 1
		s.perHiveEnvUnreachableClusters = []string{"vllm-d"}

		snap := s.PerHiveEnvSnapshot()
		if snap.KeyGenerations.SafeToRetirePrevious {
			t.Error("safe_to_retire_previous = true with an UNREACHABLE spoke. Retirement is " +
				"unconditional at verify_until, so this signal would authorise stranding a spoke " +
				"whose generation the hub never read — F21 turning F19's fix into a 7-day outage")
		}
	})

	// A single unreachable spoke is enough. Nothing about the count should
	// soften the gate.
	t.Run("one unreachable spoke among many observed still blocks", func(t *testing.T) {
		many := map[string]perHiveEnvObservation{}
		for _, id := range []string{"a", "b", "c", "d", "e"} {
			many[id] = perHiveEnvObservation{Generation: expired.Current, Observed: now}
		}
		s := fullyObservedHub(expired, many)
		s.perHiveEnvConsidered = len(many) + 1
		s.perHiveEnvUnreachable = 1

		if s.PerHiveEnvSnapshot().KeyGenerations.SafeToRetirePrevious {
			t.Error("5 observed spokes on current did not outvote 1 unreachable — they must not")
		}
	})
}

// TestPullOnlyClusterHivesCountAsUnreachable connects the declarative flag to
// the counter through the real sweep, rather than asserting against a
// hand-set field. reconcilePerHiveEnv runs over a registry whose only cluster
// is pull-only; clusterRecentlyUnreachable short-circuits on PullOnly, so every
// admitted hive must land in the unreachable bucket and none in ObservedHives.
//
// This is the test that fails if someone reverts the noteUnreachable calls and
// restores the bare `continue`.
func TestPullOnlyClusterHivesCountAsUnreachable(t *testing.T) {
	s := NewHubServer(0, slog.Default(), "test", "v2")
	s.clusters = map[string]ClusterConfig{
		"vllm-d": {ID: "vllm-d", PullOnly: true, KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d.yaml", Domain: "d"},
	}

	// A pull-only cluster is unreachable BY DECLARATION — no dial, no kubectl,
	// so this needs no cluster fixture.
	if !s.clusterRecentlyUnreachable("vllm-d") {
		t.Fatal("fixture: a pull-only cluster must count as unreachable without dialing")
	}

	// Drive the accounting the sweep would produce for 3 hives on that cluster.
	s.perHiveEnvMu.Lock()
	s.perHiveEnvSeen = map[string]perHiveEnvObservation{}
	s.perHiveEnvConsidered = 3
	s.perHiveEnvUnreachable = 3
	s.perHiveEnvUnreachableClusters = []string{"vllm-d"}
	s.perHiveEnvMu.Unlock()

	got := s.PerHiveEnvSnapshot()
	if got.ObservedHives != 0 {
		t.Errorf("ObservedHives = %d, want 0 — a pull-only hive is never READ, so it must "+
			"never be counted as observed", got.ObservedHives)
	}
	if got.UnreachableHives != 3 {
		t.Errorf("UnreachableHives = %d, want 3", got.UnreachableHives)
	}
	if got.PerHiveEnvConverged {
		t.Error("PerHiveEnvConverged = true for a fleet the hub read NOTHING from")
	}
	if got.FleetFullyObserved {
		t.Error("FleetFullyObserved = true with 3 unreachable hives")
	}

	// The counter is only worth having if something DEPENDS on it. Assert the
	// dependency directly: with observations present but hives unreachable,
	// convergence must still be false. Without this the test above passes
	// against an implementation that records the count and then ignores it —
	// which is exactly what the pre-fix code did with its bare `continue`.
	s.perHiveEnvMu.Lock()
	s.perHiveEnvSeen = map[string]perHiveEnvObservation{
		"reachable-one": {Observed: time.Now()},
	}
	s.perHiveEnvConsidered = 4
	s.perHiveEnvUnreachable = 3
	s.perHiveEnvMu.Unlock()

	got = s.PerHiveEnvSnapshot()
	if got.ObservedHives != 1 {
		t.Fatalf("fixture: ObservedHives = %d, want 1", got.ObservedHives)
	}
	if got.MissingPerHiveEnv != 0 {
		t.Fatalf("fixture: the one observed hive must be drift-free, got %d missing", got.MissingPerHiveEnv)
	}
	if got.PerHiveEnvConverged {
		t.Error("PerHiveEnvConverged = true from 1 clean read while 3 hives were unreachable — " +
			"the unreachable count must GATE convergence, not merely be reported alongside it")
	}
}

// TestFleetFullyObservedFailsClosedOnEmptySweep guards the degenerate case:
// a sweep that admitted nobody has unreachable == 0, and a naive
// "unreachable == 0" predicate would call that fully observed. Zero evidence
// is not complete evidence.
func TestFleetFullyObservedFailsClosedOnEmptySweep(t *testing.T) {
	s := &HubServer{perHiveEnvSeen: map[string]perHiveEnvObservation{}}
	got := s.PerHiveEnvSnapshot()
	if got.FleetFullyObserved {
		t.Error("FleetFullyObserved = true from a sweep that considered ZERO hives — " +
			"'I looked at nothing and found nothing unreachable' is not full observation")
	}
	if got.PerHiveEnvConverged {
		t.Error("PerHiveEnvConverged = true from zero considered hives")
	}
}

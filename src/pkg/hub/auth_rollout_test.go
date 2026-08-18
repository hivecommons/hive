package hub

import (
	"testing"
	"time"
)

// #3234: readiness telemetry for removing the N1/N2 compatibility lanes.
//
// Those lanes ARE the vulnerabilities they replaced — a fleet-wide heartbeat
// bearer does not bind identity, and a symmetric session key that verifies a
// cookie can also mint one. They exist only so the fleet could roll without a
// flag-day cutover, and they must not become permanent.
//
// The blocker to removing them was observability: "has every spoke
// re-provisioned?" had no answer short of inspecting each Deployment by hand.
// These tests pin the signal that answers it.

func rolloutHub() *HubServer {
	return &HubServer{authRolloutSeen: make(map[string]authRolloutEntry)}
}

// TestAuthRolloutFailsClosedWithNoData is the most important case: absence of
// evidence must never read as "safe to remove".
func TestAuthRolloutFailsClosedWithNoData(t *testing.T) {
	s := rolloutHub()
	got := s.AuthRolloutReadiness()
	if got.HeartbeatLaneReady {
		t.Fatal("readiness reported with ZERO observations — an operator would delete the " +
			"compat lane on a hub that has simply never seen a heartbeat, 401ing the fleet")
	}
	if got.TotalHives != 0 {
		t.Errorf("TotalHives = %d, want 0", got.TotalHives)
	}
}

// TestAuthRolloutNotReadyWhileAnyLegacy: one laggard blocks removal, and it is
// named so an operator knows which hive to re-provision.
func TestAuthRolloutNotReadyWhileAnyLegacy(t *testing.T) {
	s := rolloutHub()
	s.noteHeartbeatAuthPath("hive-rolled", true)
	s.noteHeartbeatAuthPath("hive-laggard", false)

	got := s.AuthRolloutReadiness()
	if got.HeartbeatLaneReady {
		t.Fatal("reported ready while a spoke is still on the legacy bearer")
	}
	if got.TotalHives != 2 || got.PerHiveBearer != 1 || got.LegacyBearer != 1 {
		t.Errorf("counts = total:%d per-hive:%d legacy:%d, want 2/1/1",
			got.TotalHives, got.PerHiveBearer, got.LegacyBearer)
	}
	if len(got.LegacyHives) != 1 || got.LegacyHives[0] != "hive-laggard" {
		t.Errorf("LegacyHives = %v, want [hive-laggard] — the laggard must be named", got.LegacyHives)
	}
}

// TestAuthRolloutReadyWhenAllPerHive: the positive case that unblocks #3234.
func TestAuthRolloutReadyWhenAllPerHive(t *testing.T) {
	s := rolloutHub()
	s.noteHeartbeatAuthPath("hive-a", true)
	s.noteHeartbeatAuthPath("hive-b", true)

	got := s.AuthRolloutReadiness()
	if !got.HeartbeatLaneReady {
		t.Fatalf("not ready with every hive on the per-hive bearer: %+v", got)
	}
	if len(got.LegacyHives) != 0 {
		t.Errorf("LegacyHives = %v, want empty", got.LegacyHives)
	}
}

// TestAuthRolloutIgnoresStale: a hive that stopped beating (deleted, migrated)
// must not block the removal forever.
func TestAuthRolloutIgnoresStale(t *testing.T) {
	s := rolloutHub()
	s.noteHeartbeatAuthPath("hive-current", true)
	// A long-gone hive that last beat on the legacy bearer.
	s.authRolloutSeen["hive-gone"] = authRolloutEntry{
		PerHiveBearer: false,
		LastSeen:      time.Now().Add(-authRolloutStaleAfter - time.Hour),
	}

	got := s.AuthRolloutReadiness()
	if got.TotalHives != 1 {
		t.Errorf("TotalHives = %d, want 1 (the stale hive must be ignored)", got.TotalHives)
	}
	if !got.HeartbeatLaneReady {
		t.Error("a hive that has not beaten in over a day blocked readiness forever")
	}
}

// TestAuthRolloutDoesNotLatch: a spoke that rolls BACK must flip the signal
// back to not-ready. Latching "has ever been per-hive" would make the fleet
// look ready when it no longer is.
func TestAuthRolloutDoesNotLatch(t *testing.T) {
	s := rolloutHub()
	s.noteHeartbeatAuthPath("hive-a", true)
	if !s.AuthRolloutReadiness().HeartbeatLaneReady {
		t.Fatal("setup: expected ready")
	}
	// Same hive rolls back to an older image and presents the legacy bearer.
	s.noteHeartbeatAuthPath("hive-a", false)
	if s.AuthRolloutReadiness().HeartbeatLaneReady {
		t.Fatal("readiness LATCHED — a rolled-back spoke still reads as ready, which is " +
			"exactly when removing the lane would break it")
	}
}

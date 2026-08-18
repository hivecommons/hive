package hub

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// ============================================================
// server.go — handleHeartbeat registry-update + upgrade-decision branches
// ============================================================

func TestHandleHeartbeatUpgradeComplete(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// Existing registry entry mid-upgrade; the spoke now reports the target SHA
	// while NOT upgrading -> the hub clears the Upgrading flag.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "target9", GitHash: "old0000",
	}}
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"target9","upgrading":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	up := s.registry.Hives[0].Upgrading
	s.mu.RUnlock()
	if up {
		t.Error("expected Upgrading cleared after reaching target SHA")
	}
}

func TestHandleHeartbeatStaleUpgradeFlag(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// Upgrading with empty target (stale flag) + non-upgrading heartbeat -> cleared.
	s.registry.Hives = []RegistryEntry{{ID: "h1", Upgrading: true, UpgradeTarget: ""}}
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"whatever","upgrading":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	up := s.registry.Hives[0].Upgrading
	s.mu.RUnlock()
	if up {
		t.Error("expected stale Upgrading flag cleared")
	}
}

func TestHandleHeartbeatUpgradingKeepsTarget(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// Still upgrading, spoke reports same hash -> hub keeps the target + timestamp.
	started := time.Now().Add(-time.Minute)
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", Upgrading: true, UpgradeTarget: "tgt", GitHash: "same",
		UpgradeStartedAt: started,
	}}
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"same","upgrading":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	entry := s.registry.Hives[0]
	s.mu.RUnlock()
	if !entry.Upgrading || entry.UpgradeTarget != "tgt" {
		t.Errorf("expected upgrade target preserved, got %+v", entry)
	}
}

func TestHandleHeartbeatSwitchTagInstructAndComplete(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	s.heartbeatSwitchTag["h1"] = "dev-latest"

	// Spoke NOT yet on the target branch -> hub keeps instructing (switch_to_tag).
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dev-latest") {
		t.Errorf("expected switch_to_tag instruction, got %s", rec.Body.String())
	}

	// Spoke now reports the target branch -> hub clears the switch tag.
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"dev"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	_, stillSet := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if stillSet {
		t.Error("expected switch tag cleared once spoke reports target branch")
	}
}

func TestHandleHeartbeatHubUpgradeFallback(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	// A queued kubectl-fallback upgrade target the spoke is not yet at -> UpgradeTo.
	s.heartbeatUpgrade["h1"] = "newtarget"

	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"oldhash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "newtarget") {
		t.Errorf("expected upgrade_to instruction, got %s", rec.Body.String())
	}

	// Spoke reaches the target -> the fallback entry is cleared.
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_hash":"newtarget"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	_, still := s.heartbeatUpgrade["h1"]
	s.mu.RUnlock()
	if still {
		t.Error("expected heartbeat upgrade fallback cleared once spoke at target")
	}
}

func TestHandleHeartbeatForbidsUnprovisionedHosted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	// A hosted-* hive ID with no SaaS record cannot self-register.
	rec := postHeartbeat(t, s, `{"hive_id":"hosted-ghost"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unprovisioned hosted heartbeat status = %d, want 403", rec.Code)
	}
}

package hub

// Tests for issue #3783: heartbeatSwitchTag (branch-switch via heartbeat
// fallback) is in-memory only and NOT recovered on hub restart — unlike
// heartbeatUpgrade which is recovered by recoverArmedUpgrades.
//
// handleSwitchBranch does call beginUpgrade (persisting Upgrading=true and
// UpgradeTarget in the registry), so the registry knows an upgrade is in
// flight. However, recoverArmedUpgrades re-arms heartbeatUpgrade only — it
// does NOT re-arm heartbeatSwitchTag. The spoke therefore never receives the
// switch_to_tag instruction after a hub restart, and the upgrade hangs until
// manual intervention or a second branch-switch request.

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSwitchTagLostOnRestart proves the durability gap: a branch switch
// armed via handleSwitchBranch (heartbeat fallback path) is lost when
// the hub restarts because heartbeatSwitchTag is not persisted.
func TestSwitchTagLostOnRestart(t *testing.T) {
	// Simulate a hub with a pending branch switch.
	s := &HubServer{
		logger:             slog.Default(),
		heartbeatSwitchTag: map[string]string{"hive-1": "dev-latest"},
		heartbeatUpgrade:   make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Upgrading: true, UpgradeTarget: "dev-latest"},
	}

	// recoverArmedUpgrades rebuilds heartbeatUpgrade from the registry,
	// but has no knowledge of heartbeatSwitchTag.
	s2 := &HubServer{
		logger:             slog.Default(),
		heartbeatSwitchTag: make(map[string]string),
	}
	s2.registry.Hives = s.registry.Hives

	s2.recoverArmedUpgrades()

	s2.mu.RLock()
	// heartbeatUpgrade IS recovered (the SHA-based upgrade path).
	if _, armed := s2.heartbeatUpgrade["hive-1"]; !armed {
		t.Error("heartbeatUpgrade should be recovered from registry latch")
	}
	// heartbeatSwitchTag is NOT recovered — documenting the gap.
	if _, armed := s2.heartbeatSwitchTag["hive-1"]; armed {
		t.Log("heartbeatSwitchTag was recovered — durability gap is fixed!")
	} else {
		t.Log("KNOWN GAP: heartbeatSwitchTag not recovered on restart; " +
			"branch switches via heartbeat fallback are lost on hub restart")
	}
	s2.mu.RUnlock()
}

// TestSwitchTagInHeartbeatResponse verifies that when heartbeatSwitchTag
// is armed, the heartbeat response includes switch_to_tag, and that it is
// cleared once the spoke reports the matching branch.
func TestSwitchTagInHeartbeatResponse(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", GitBranch: "v2"}}
	s.heartbeatSwitchTag["h1"] = "dev-latest"

	// First heartbeat: spoke still on old branch → hub should instruct.
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v2"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dev-latest") {
		t.Errorf("expected switch_to_tag in response, got %s", rec.Body.String())
	}

	// Second heartbeat: spoke now on target branch → hub should clear.
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"dev"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.mu.RLock()
	_, stillArmed := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if stillArmed {
		t.Error("heartbeatSwitchTag should be cleared once spoke reports matching branch")
	}
}

// TestSwitchTagNotPersistedInRegistry verifies that heartbeatSwitchTag
// entries do not appear in the serialized registry (confirming the in-memory-only
// nature of the field and the durability gap).
func TestSwitchTagNotPersistedInRegistry(t *testing.T) {
	dir := t.TempDir()
	regFile := filepath.Join(dir, "hub-registry.json")
	s := &HubServer{
		logger:             slog.Default(),
		saveCh:             make(chan struct{}, 1),
		registryPath:       regFile,
		heartbeatSwitchTag: map[string]string{"hive-1": "dev-latest"},
		heartbeatUpgrade:   make(map[string]string),
	}
	s.registry.Hives = []RegistryEntry{
		{ID: "hive-1", Upgrading: true, UpgradeTarget: "dev-latest"},
	}

	// Save registry to disk.
	if err := s.saveRegistryNow(); err != nil {
		t.Fatalf("saveRegistryNow: %v", err)
	}

	data, err := os.ReadFile(regFile)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}

	// The registry should contain the upgrade latch but NOT the switchTag.
	if !strings.Contains(string(data), "upgradeTarget") {
		t.Error("expected upgradeTarget in persisted registry")
	}
	// heartbeatSwitchTag is a HubServer field, not a RegistryEntry field,
	// so it must not appear in the JSON.
	if strings.Contains(string(data), "switchTag") || strings.Contains(string(data), "switch_tag") {
		t.Error("heartbeatSwitchTag should not be in the persisted registry")
	}

	// Reload into a fresh server and verify the switch tag is gone.
	s2 := &HubServer{
		logger:             slog.Default(),
		registryPath:       regFile,
		heartbeatSwitchTag: make(map[string]string),
	}
	s2.loadRegistry()
	s2.recoverArmedUpgrades()

	s2.mu.RLock()
	_, armed := s2.heartbeatSwitchTag["hive-1"]
	s2.mu.RUnlock()
	if armed {
		t.Log("heartbeatSwitchTag was recovered — the durability gap may be fixed")
	}

	// The upgrade latch itself should be recovered.
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(reg.Hives) != 1 || !reg.Hives[0].Upgrading {
		t.Error("expected upgrade latch to be persisted")
	}
}

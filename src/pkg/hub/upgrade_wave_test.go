package hub

import (
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// TestTriggerAutoUpgradesWaveBound verifies the per-cluster upgrade wave: with
// N eligible behind hives on one cluster and a wave size smaller than N, one
// trigger cycle arms exactly waveSize upgrades; the rest board later waves as
// in-flight upgrades clear.
func TestTriggerAutoUpgradesWaveBound(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	resetCommitOrderState(t)
	stubCommitCompare(func(base, head string, logger *slog.Logger) (string, error) {
		return "behind", nil
	})
	t.Setenv("HIVE_UPGRADE_WAVE_SIZE", "3")
	setLatestSHAForBranchForTest(t, "v4", "newsha123")

	cluster := ClusterConfig{ID: "wave-c1", InCluster: true}
	s := &HubServer{
		logger:           slog.Default(),
		hubSecret:        testHubSecret,
		heartbeatUpgrade: make(map[string]string),
		clusters:         map[string]ClusterConfig{"wave-c1": cluster},
	}
	beat := time.Now().UTC().Format(time.RFC3339)
	const total = 8
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("wave-h%d", i)
		saveSaaSHive(&SaaSHive{ID: id, Owner: "alice", AutoUpgrade: true, Status: "running", ClusterID: "wave-c1"})
		s.registry.Hives = append(s.registry.Hives, RegistryEntry{
			ID: id, GitBranch: "v4", GitHash: "oldsha", LastHeartbeat: beat,
		})
	}

	s.triggerAutoUpgrades()

	s.mu.RLock()
	armed := len(s.heartbeatUpgrade)
	s.mu.RUnlock()
	if armed != 3 {
		t.Fatalf("first wave armed %d upgrades, want wave size 3", armed)
	}

	// A second cycle with the wave still in flight must not start more.
	s.triggerAutoUpgrades()
	s.mu.RLock()
	armed = len(s.heartbeatUpgrade)
	s.mu.RUnlock()
	if armed != 3 {
		t.Fatalf("second cycle grew in-flight to %d, want still 3 (wave full)", armed)
	}

	// Wave slots free as spokes converge: mark the armed three landed and the
	// next cycle boards the next wave.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].Upgrading {
			s.registry.Hives[i].Upgrading = false
			s.registry.Hives[i].UpgradeTarget = ""
			s.registry.Hives[i].GitHash = "newsha123"
		}
	}
	s.mu.Unlock()

	s.triggerAutoUpgrades()
	s.mu.RLock()
	nowUpgrading := 0
	for _, reg := range s.registry.Hives {
		if reg.Upgrading {
			nowUpgrading++
		}
	}
	s.mu.RUnlock()
	if nowUpgrading != 3 {
		t.Fatalf("after wave 1 landed, wave 2 has %d in flight, want 3", nowUpgrading)
	}
}

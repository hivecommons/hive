package agent

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestReconcileAgentsAddsReplicaWithUIDAndRemovesExtra(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {ID: "scanner", Enabled: true}}, slog.Default(), ProjectContext{})
	m.uidMap = NewUIDMap()
	// ReconcileAgents persists the UID map to UIDMapPath when one is live;
	// keep the save out of the binary-wide TestMain path (#5580).
	stubUIDMapPath(t)
	m.ReconcileAgents(map[string]config.AgentConfig{
		"scanner":   {ID: "scanner", Enabled: true, ReplicaIndex: 1, ReplicaCount: 2},
		"scanner-2": {ID: "scanner-2", Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2},
	})
	if _, ok := m.agents["scanner-2"]; !ok {
		t.Fatal("scanner-2 not added")
	}
	if got := m.agents["scanner-2"].UID; got == 0 {
		t.Fatal("scanner-2 UID was not allocated")
	}
	m.ReconcileAgents(map[string]config.AgentConfig{"scanner": {ID: "scanner", Enabled: true, ReplicaIndex: 1, ReplicaCount: 1}})
	if _, ok := m.agents["scanner-2"]; ok {
		t.Fatal("scanner-2 not removed")
	}
}

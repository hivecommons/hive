package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestBuildAgentsIncludesReplicaMetadataAndBaseCadence(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":   {ID: "scanner", Enabled: true, Replicas: 2, ReplicaIndex: 1, ReplicaCount: 2},
			"scanner-2": {ID: "scanner-2", Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]config.Cadence{"scanner": "10m"}},
		}},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner":   {Name: "scanner", ID: "scanner", Config: cfg.Agents["scanner"], State: agent.StateRunning},
		"scanner-2": {Name: "scanner-2", ID: "scanner-2", Config: cfg.Agents["scanner-2"], State: agent.StateRunning},
	}
	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	var replica *FrontendAgent
	for i := range agents {
		if agents[i].Name == "scanner-2" {
			replica = &agents[i]
		}
	}
	if replica == nil {
		t.Fatal("scanner-2 missing")
	}
	if replica.ReplicaBase != "scanner" || replica.ReplicaIndex != 2 || replica.ReplicaCount != 2 {
		t.Fatalf("replica metadata = %+v", replica)
	}
	if replica.Cadence != "10m" {
		t.Fatalf("replica cadence = %q, want 10m", replica.Cadence)
	}
}

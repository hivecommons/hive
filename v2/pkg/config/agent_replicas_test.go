package config

import (
	"strings"
	"testing"
)

func TestExpandAgentReplicasMaterializesDerivedNames(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{
		"scanner": {ID: "scanner", Backend: "copilot", Model: "auto", Enabled: true, Replicas: 3},
	}}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatalf("ExpandAgentReplicas() error = %v", err)
	}
	for _, name := range []string{"scanner", "scanner-2", "scanner-3"} {
		if _, ok := cfg.Agents[name]; !ok {
			t.Fatalf("missing materialized agent %s", name)
		}
	}
	if got := cfg.Agents["scanner-2"]; got.ReplicaOf != "scanner" || got.ReplicaIndex != 2 || got.ReplicaCount != 3 || got.ID != "scanner-2" {
		t.Fatalf("scanner-2 metadata = %+v", got)
	}
	if got := cfg.Agents["scanner-3"].BeadsDir; got != "/data/beads/scanner-3" {
		t.Fatalf("scanner-3 beads dir = %q", got)
	}
}

func TestExpandAgentReplicasRejectsCollisionsAndCap(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{
		"scanner":   {Replicas: 2},
		"scanner-2": {Replicas: 1},
	}}
	if err := cfg.ExpandAgentReplicas(); err == nil || !strings.Contains(err.Error(), "scanner-2") {
		t.Fatalf("collision error = %v", err)
	}

	cfg = &Config{Agents: map[string]AgentConfig{"scanner": {Replicas: MaxAgentReplicas + 1}}}
	if err := cfg.ExpandAgentReplicas(); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Fatalf("cap error = %v", err)
	}
}

func TestExpandAgentReplicasOneIsZeroMigration(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{"scanner": {Replicas: 1}}}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatalf("ExpandAgentReplicas() error = %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len agents = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents["scanner"].ReplicaOf != "" || cfg.Agents["scanner"].ReplicaIndex != 1 {
		t.Fatalf("base metadata = %+v", cfg.Agents["scanner"])
	}
}

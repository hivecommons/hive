package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestAgentStatusCarriesContinuousBudgetAndCounters(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {Backend: "copilot", Enabled: true, Continuous: true},
	}}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Name: "scanner", Config: cfg.Agents["scanner"], State: agent.StateRunning},
	}
	govState := governor.State{
		Mode: governor.ModeIdle,
		Continuous: map[string]governor.ContinuousState{
			"scanner": {Blocked: "budget", Kicks: 3, TokensConsumed: 42},
		},
	}

	agents := buildAgents(statuses, cfg, govState)
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].ContinuousBlocked != "budget" {
		t.Fatalf("continuousBlocked = %q, want budget", agents[0].ContinuousBlocked)
	}
	if agents[0].ContinuousKicks != 3 || agents[0].ContinuousTokens != 42 {
		t.Fatalf("continuous counters = kicks %d tokens %d, want 3/42", agents[0].ContinuousKicks, agents[0].ContinuousTokens)
	}
}

package dashboard

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestAgentStatusCarriesContinuousBudgetAndCounters(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {Backend: "copilot", Enabled: true, Continuous: true},
	}, Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "15m"}},
	}}}
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
	if want := []string{"idle", "busy", "surge"}; !reflect.DeepEqual(agents[0].ContinuousModes, want) {
		t.Fatalf("continuousModes = %v, want %v", agents[0].ContinuousModes, want)
	}
}

func TestAgentStatusContinuousNotInMode(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"scanner": {Backend: "copilot", Enabled: true}},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"busy":  {Cadences: map[string]config.Cadence{"scanner": "15m"}},
			"surge": {Cadences: map[string]config.Cadence{"scanner": "continuous"}},
		}},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {Name: "scanner", Config: cfg.Agents["scanner"], State: agent.StateRunning},
	}
	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeBusy})
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	if agents[0].Continuous {
		t.Fatal("busy mode must not report continuous for surge-only agent")
	}
	if agents[0].ContinuousBlocked != "not_in_mode" {
		t.Fatalf("continuousBlocked = %q, want not_in_mode", agents[0].ContinuousBlocked)
	}
}

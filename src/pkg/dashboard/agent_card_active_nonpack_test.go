package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// TestBuildAgentsIncludesActiveAgentOutsideCurrentPack reproduces #6488. An
// operator may explicitly enable an agent that is not part of the current ACMM
// pack (supervisor at L1 here), including when that agent uses a named model
// gateway. The manager reports the process as running, so filtering it out of
// the status payload leaves the dashboard's Agents section completely blank.
//
// Inactive non-pack entries must remain hidden: those are the higher-level
// "ghost" cards the pack filter was introduced to suppress.
func TestBuildAgentsIncludesActiveAgentOutsideCurrentPack(t *testing.T) {
	level := 1
	supervisorCfg := config.AgentConfig{
		Backend:     "unsloth-local",
		Model:       "unsloth/gemma-4-E4B-it-qat-GGUF",
		Enabled:     true,
		DisplayName: "Supervisor",
	}
	ghostCfg := config.AgentConfig{
		Backend: "unsloth-local",
		Enabled: true,
		Paused:  true,
	}
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"supervisor": supervisorCfg,
			"scanner":    ghostCfg,
		},
		Governor: config.GovernorConfig{
			Gateways: []config.GatewayConfig{{
				Name:     "unsloth-local",
				Kind:     config.GatewayKindCustom,
				Endpoint: "http://host.docker.internal:8888",
			}},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"supervisor": {
			Name:         "supervisor",
			Config:       supervisorCfg,
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"scanner": {
			Name:          "scanner",
			Config:        ghostCfg,
			State:         agent.StatePaused,
			Paused:        true,
			PausedTrigger: "acmm-pack",
			OutputBuffer:  agent.NewRingBuffer(10),
		},
	}

	got := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(got) != 1 {
		t.Fatalf("agents = %v, want only the active non-pack supervisor", agentNamesFromFrontend(got))
	}
	if got[0].Name != "supervisor" {
		t.Fatalf("agent name = %q, want supervisor", got[0].Name)
	}
	if got[0].CLI != "unsloth-local" {
		t.Errorf("agent CLI = %q, want configured gateway name unsloth-local", got[0].CLI)
	}
}

// TestBuildAgentsWithHidden_ReportsPackInactiveGhost is #6581's diagnostic
// companion to the test above: it exercises buildAgentsWithHidden on the
// EXACT same fixture (an active out-of-pack supervisor beside an inactive
// pack-paused "ghost" scanner) and asserts the hidden-agent list is precise —
// the active supervisor must never appear in it, and the suppressed ghost
// must, tagged with the stable "pack-inactive" reason. Before this pairing
// existed, an operator who still saw an empty Agents section after upgrading
// past #6503 (as reported in #6581) had no way to tell, from /api/status
// alone, whether the hive even attempted to surface a given agent, or why it
// didn't.
func TestBuildAgentsWithHidden_ReportsPackInactiveGhost(t *testing.T) {
	level := 1
	supervisorCfg := config.AgentConfig{
		Backend: "unsloth-local",
		Enabled: true,
	}
	ghostCfg := config.AgentConfig{
		Backend: "unsloth-local",
		Enabled: true,
		Paused:  true,
	}
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"supervisor": supervisorCfg,
			"scanner":    ghostCfg,
		},
		Governor: config.GovernorConfig{
			Gateways: []config.GatewayConfig{{
				Name:     "unsloth-local",
				Kind:     config.GatewayKindCustom,
				Endpoint: "http://host.docker.internal:8888",
			}},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"supervisor": {
			Name:         "supervisor",
			Config:       supervisorCfg,
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"scanner": {
			Name:          "scanner",
			Config:        ghostCfg,
			State:         agent.StatePaused,
			Paused:        true,
			PausedTrigger: "acmm-pack",
			OutputBuffer:  agent.NewRingBuffer(10),
		},
	}

	agents, hidden := buildAgentsWithHidden(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(agents) != 1 || agents[0].Name != "supervisor" {
		t.Fatalf("agents = %v, want only supervisor", agentNamesFromFrontend(agents))
	}
	if len(hidden) != 1 {
		t.Fatalf("hidden = %+v, want exactly one entry for scanner", hidden)
	}
	if hidden[0].Name != "scanner" {
		t.Errorf("hidden[0].Name = %q, want scanner", hidden[0].Name)
	}
	if hidden[0].Reason != hiddenReasonPackInactive {
		t.Errorf("hidden[0].Reason = %q, want %q", hidden[0].Reason, hiddenReasonPackInactive)
	}
	for _, h := range hidden {
		if h.Name == "supervisor" {
			t.Fatalf("active supervisor must never be reported as hidden: %+v", hidden)
		}
	}
}

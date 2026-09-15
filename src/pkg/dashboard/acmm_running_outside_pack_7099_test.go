package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// TestBuildAgentsWithHidden_RunningCustomAgentOutsideAnyPackVisible7099 pins the
// server contract behind #7099. A custom agent named "review" is enabled and
// running but belongs to NO ACMM pack roster at any level. The server must
// publish it in status.agents (via the activeOutsidePack escape hatch) and must
// NOT record it in hiddenAgents — otherwise the dashboard has no card to render
// and the operator cannot reach the agent's terminal/Login controls the
// watchdog is simultaneously asking them to use.
//
// The pack members (supervisor, scanner) stay visible normally; the point of
// this fixture is the running non-roster agent.
func TestBuildAgentsWithHidden_RunningCustomAgentOutsideAnyPackVisible7099(t *testing.T) {
	level := 5
	supervisorCfg := config.AgentConfig{Backend: "unsloth-local", Enabled: true}
	reviewCfg := config.AgentConfig{Backend: "unsloth-local", Enabled: true, DisplayName: "Review"}
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"supervisor": supervisorCfg,
			"review":     reviewCfg,
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
		"review": {
			Name:         "review",
			Config:       reviewCfg,
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	agents, hidden := buildAgentsWithHidden(statuses, cfg, governor.State{Mode: governor.ModeIdle})

	names := map[string]bool{}
	for _, a := range agents {
		names[a.Name] = true
	}
	if !names["review"] {
		t.Fatalf("running custom agent 'review' outside every pack roster must be published, got agents=%v", agentNamesFromFrontend(agents))
	}
	for _, h := range hidden {
		if h.Name == "review" {
			t.Fatalf("running out-of-pack agent 'review' must never be recorded as hidden: %+v", hidden)
		}
	}
}

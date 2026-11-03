package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/housestyle"
)

// The agent card's "style" field is read back out of the last kick's HOUSE
// STYLE header, so the dashboard reports exactly what the agent was sent
// without a second channel from the scheduler.
func TestFrontendAgentStyleSourceFromLastKick(t *testing.T) {
	agentCfg := config.AgentConfig{Enabled: true}
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"repo-styled": agentCfg, "fresh": agentCfg},
		},
	}
	kick := "[agent:repo-styled]\n\n" + housestyle.Resolution{Text: "rules", Source: housestyle.SourceRepo}.Section() + "\n\nbody"
	statuses := map[string]*crux.Process{
		"repo-styled": {Name: "repo-styled", Config: agentCfg, State: crux.StateRunning, OutputBuffer: cruxtest.NewRingBuffer(10), LastKickMessage: kick},
		"fresh":       {Name: "fresh", Config: agentCfg, State: crux.StateRunning, OutputBuffer: cruxtest.NewRingBuffer(10)},
	}
	agents, _ := buildAgentsWithHidden(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	got := map[string]string{}
	for _, a := range agents {
		got[a.Name] = a.StyleSource
	}
	if got["repo-styled"] != "repo" || got["fresh"] != "" {
		t.Fatalf("styleSource = %v", got)
	}
}

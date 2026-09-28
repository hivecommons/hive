package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// A hive restart relaunches agents on a 15s stagger. Until an agent reaches
// its pane its State is still "stopped", which the SPA painted as down-red on
// every restart. The snapshot's Starting flag must reach /api/status so the
// UI can show the boot window as "starting" — and it must never be set on an
// agent that is actually running.
func TestBuildAgents_StartingFlagOnlyWhileNotRunning(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Enabled: true},
			"quality": {Enabled: true},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:         "scanner",
			Config:       cfg.Agents["scanner"],
			State:        agent.StateStopped,
			Starting:     true,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"quality": {
			Name:         "quality",
			Config:       cfg.Agents["quality"],
			State:        agent.StateRunning,
			Starting:     true,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	agents := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	got := map[string]FrontendAgent{}
	for _, a := range agents {
		got[a.Name] = a
	}
	if !got["scanner"].Starting {
		t.Fatalf("stopped agent in boot stagger must publish starting=true: %+v", got["scanner"])
	}
	if got["quality"].Starting {
		t.Fatalf("running agent must never publish starting=true: %+v", got["quality"])
	}
}

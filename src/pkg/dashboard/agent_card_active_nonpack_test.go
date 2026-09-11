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

// TestBuildAgentsWithHidden_RuntimePausedGhostNotResurrectedByConfigPass
// guards the fix for #6581/#6488 (buildMissingRuntimeAgent): that fix added a
// second pass over cfg.Agents to surface agents with NO runtime status entry
// at all, using agentCfg.Paused (a config-level signal) as its own pack-gate
// check. A ghost agent IS present in the runtime `statuses` map and is hidden
// by the first pass because its runtime proc is paused-outside-pack — but its
// config-level Paused flag can independently be false (e.g. runtime pause was
// never persisted back to config). Without marking every runtime-status name
// "seen" up front, the config pass would re-evaluate that same ghost using
// the looser agentCfg.Paused signal, find it false, and re-add it — resulting
// in the agent appearing as BOTH a normal visible card (because `statuses`
// still has its live proc) and an entry in HiddenAgents. This must never
// happen: an agent the pack gate hid stays hidden regardless of the second
// pass.
func TestBuildAgentsWithHidden_RuntimePausedGhostNotResurrectedByConfigPass(t *testing.T) {
	level := 1
	ghostCfg := config.AgentConfig{
		Backend: "unsloth-local",
		Enabled: true,
		Paused:  false, // config-level Paused NOT set, unlike the runtime proc below
	}
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"scanner": ghostCfg,
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
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want none: a pack-paused ghost with a live (hidden) runtime entry must not be resurrected by the config-only pass", agentNamesFromFrontend(agents))
	}
	if len(hidden) != 1 || hidden[0].Name != "scanner" || hidden[0].Reason != hiddenReasonPackInactive {
		t.Fatalf("hidden = %+v, want exactly one pack-inactive entry for scanner", hidden)
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

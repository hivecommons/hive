package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// #6652 reopened #6581: agent cards were still missing on a hive running a
// build that already contained both the #6581 fix and the #6589 diagnostic.
// The reason the diagnostic could not settle it is covered here.
//
// buildAgentsWithHidden has two passes. The runtime pass reports every card it
// omits through HiddenAgents. The config-only pass — the one that handles
// agents present in the config but absent from the runtime status map — used
// to drop agents with a bare `continue`, recording nothing. So a hive whose
// agents were all gated out by that second pass rendered no cards AND reported
// an empty hiddenAgents list, which is indistinguishable from the builder
// never having seen the config at all. Every omission must name its reason.
func TestBuildAgentsWithHidden_ConfigOnlyPassReportsEveryOmission(t *testing.T) {
	level := 1 // pack level 1 allows only guide and brainstorm

	cases := []struct {
		name       string
		agentName  string
		agentCfg   config.AgentConfig
		wantReason string
	}{
		{
			name:       "disabled in config",
			agentName:  "quality",
			agentCfg:   config.AgentConfig{Backend: "claude", Enabled: false},
			wantReason: hiddenReasonDisabled,
		},
		{
			// telemetry is gated by AgentAvailableAtACMMLevel, which at level 1
			// is below the operability-agent minimum.
			name:       "below the ACMM operability gate",
			agentName:  "telemetry",
			agentCfg:   config.AgentConfig{Backend: "claude", Enabled: true},
			wantReason: hiddenReasonBelowACMMGate,
		},
		{
			name:       "outside the ACMM pack and paused",
			agentName:  "quality",
			agentCfg:   config.AgentConfig{Backend: "claude", Enabled: true, Paused: true},
			wantReason: hiddenReasonPackInactive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				ACMMLevel: &level,
				Agents:    map[string]config.AgentConfig{tc.agentName: tc.agentCfg},
			}
			// Deliberately empty: this is the config-only pass, i.e. an agent
			// the runtime never registered.
			statuses := map[string]*agent.AgentProcess{}

			agents, hidden := buildAgentsWithHidden(statuses, cfg, governor.State{Mode: governor.ModeIdle})

			if len(agents) != 0 {
				t.Fatalf("agents = %v, want none", agentNamesFromFrontend(agents))
			}
			if len(hidden) != 1 {
				t.Fatalf("hidden = %+v, want exactly one entry explaining why %q has no card", hidden, tc.agentName)
			}
			if hidden[0].Name != tc.agentName || hidden[0].Reason != tc.wantReason {
				t.Fatalf("hidden[0] = %+v, want {Name:%q Reason:%q}", hidden[0], tc.agentName, tc.wantReason)
			}
		})
	}
}

// An agent that legitimately gets a card must not also be reported as hidden:
// a reason list that names a visible agent is worse than no list, because it
// sends whoever reads it looking for a gate that never fired.
func TestBuildAgentsWithHidden_ConfigOnlyPassKeepsAllowedAgentVisible(t *testing.T) {
	level := 1
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"guide": {Backend: "claude", Enabled: true},
		},
	}

	agents, hidden := buildAgentsWithHidden(map[string]*agent.AgentProcess{}, cfg, governor.State{Mode: governor.ModeIdle})

	names := agentNamesFromFrontend(agents)
	if len(names) != 1 || names[0] != "guide" {
		t.Fatalf("agents = %v, want exactly [guide]: an enabled in-pack agent with no runtime entry still needs a card", names)
	}
	if len(hidden) != 0 {
		t.Fatalf("hidden = %+v, want empty: guide is visible", hidden)
	}
}

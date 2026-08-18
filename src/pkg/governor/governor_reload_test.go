package governor

import (
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestUpdateConfigAndAgentsRefreshesNormalCadenceSnapshotAtomically(t *testing.T) {
	initialConfig := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"worker": "1ns", "quality": "1ns"}},
	}}
	g := New(initialConfig, map[string]config.AgentConfig{
		"worker":  {Enabled: true, Role: "developer"},
		"quality": {Enabled: true, Role: "quality"},
	}, slog.Default())

	if due := g.Evaluate(0, 0, 0, 0); !slices.Contains(due, "worker") || !slices.Contains(due, "quality") {
		t.Fatalf("initial enabled agents were not both eligible: %v", due)
	}

	reloadedConfig := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"worker": "1ns", "quality": "1ns"}},
	}}
	reloadedAgents := map[string]config.AgentConfig{
		"worker": {Enabled: true, Role: "reviewer", OnDemand: true},
	}
	g.UpdateConfigAndAgents(reloadedConfig, reloadedAgents)

	// Mutating the watcher's map after the handoff must not mutate the
	// Governor snapshot used by normal cadence or Visual admission.
	worker := reloadedAgents["worker"]
	worker.Role = "mutated-after-reload"
	worker.OnDemand = false
	reloadedAgents["worker"] = worker

	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("reloaded on-demand/disabled agents must not be kicked: %v", due)
	}

	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.agents) != 1 || g.agents["worker"].Role != "reviewer" || !g.agents["worker"].OnDemand {
		t.Fatalf("Governor did not retain the exact reloaded enabled-agent snapshot: %#v", g.agents)
	}
	if _, exists := g.state.Cadences["quality"]; exists {
		t.Fatal("disabled agent retained a stale normal cadence after reload")
	}
	if g.state.LastEval.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("normal Governor evaluation did not continue after agent reload")
	}
}

func TestGovernorAgentSnapshotDeepClonesAtConstructionAndReload(t *testing.T) {
	initialInput := map[string]config.AgentConfig{"quality": richAgentConfig("initial")}
	initialExpected := richAgentConfig("initial")
	g := New(config.GovernorConfig{}, initialInput, slog.Default())
	mutateRichAgentConfig(initialInput, "quality", "changed-after-new")
	assertAgentSnapshot(t, g, initialExpected)

	reloadedInput := map[string]config.AgentConfig{"quality": richAgentConfig("reloaded")}
	reloadedExpected := richAgentConfig("reloaded")
	g.UpdateConfigAndAgents(config.GovernorConfig{}, reloadedInput)
	mutateRichAgentConfig(reloadedInput, "quality", "changed-after-update")
	assertAgentSnapshot(t, g, reloadedExpected)
}

func TestGovernorConfigSnapshotDeepClonesAtConstructionAndReload(t *testing.T) {
	initialInput := richGovernorConfig("initial")
	initialExpected := richGovernorConfig("initial")
	g := New(initialInput, nil, slog.Default())
	mutateRichGovernorConfig(&initialInput, "changed-after-new")
	assertGovernorConfigSnapshot(t, g, initialExpected)

	reloadedInput := richGovernorConfig("reloaded")
	reloadedExpected := richGovernorConfig("reloaded")
	g.UpdateConfig(reloadedInput)
	mutateRichGovernorConfig(&reloadedInput, "changed-after-update")
	assertGovernorConfigSnapshot(t, g, reloadedExpected)

	combinedInput := richGovernorConfig("combined")
	combinedExpected := richGovernorConfig("combined")
	g.UpdateConfigAndAgents(combinedInput, map[string]config.AgentConfig{"quality": {Enabled: true}})
	mutateRichGovernorConfig(&combinedInput, "changed-after-combined-update")
	assertGovernorConfigSnapshot(t, g, combinedExpected)
}

func richGovernorConfig(marker string) config.GovernorConfig {
	return config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {
				Threshold: 1,
				Cadences:  map[string]config.Cadence{"quality": config.Cadence(marker + "-cadence")},
			},
		},
		Labels: config.LabelsConfig{Exempt: []string{marker + "-label"}},
		Sensing: config.SensingConfig{
			GHRatePatterns:     []string{marker + "-rate"},
			CLIExcludePatterns: []string{marker + "-exclude"},
			LoginPatterns:      []string{marker + "-login"},
		},
	}
}

func mutateRichGovernorConfig(governorConfig *config.GovernorConfig, marker string) {
	mode := governorConfig.Modes["idle"]
	mode.Threshold = 99
	mode.Cadences["quality"] = config.Cadence(marker)
	governorConfig.Modes["idle"] = mode
	governorConfig.Modes[marker] = config.ModeConfig{Cadences: map[string]config.Cadence{marker: config.Cadence(marker)}}
	governorConfig.Labels.Exempt[0] = marker
	governorConfig.Sensing.GHRatePatterns[0] = marker
	governorConfig.Sensing.CLIExcludePatterns[0] = marker
	governorConfig.Sensing.LoginPatterns[0] = marker
}

func assertGovernorConfigSnapshot(t *testing.T, g *Governor, expected config.GovernorConfig) {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !reflect.DeepEqual(g.cfg, expected) {
		t.Fatalf("Governor config snapshot changed through caller-owned nested state:\nactual:   %#v\nexpected: %#v", g.cfg, expected)
	}
}

func richAgentConfig(marker string) config.AgentConfig {
	includeRepos := true
	channelEnabled := true
	return config.AgentConfig{
		Enabled:        true,
		Role:           marker + "-role",
		Aliases:        []string{marker + "-alias"},
		LaneKeywords:   []string{marker + "-lane"},
		DetectKeywords: []string{marker + "-detect"},
		IncludeRepos:   &includeRepos,
		StatsDisplay:   []config.StatsDisplayEntry{{Key: marker + "-stat", Label: marker + "-label"}},
		ACMMLevels:     []int{3},
		Channels: []config.ChannelConfig{{
			Type: marker + "-channel", Enabled: &channelEnabled,
			Events: []string{marker + "-event"}, Patterns: []string{marker + "-pattern"},
			Match: map[string]string{"key": marker + "-match"}, Repos: []string{marker + "-repo"},
		}},
		Tools: &config.ToolsConfig{
			Preset: marker + "-preset",
			Rules:  []config.ToolRule{{Pattern: marker + "-tool", Action: "allow", Reason: marker + "-reason"}},
		},
		Connections: []config.ConnectionConfig{{
			Name: marker + "-connection", Type: "mcp",
			Auth:    &config.ConnectionAuth{Type: marker + "-auth", EnvVar: marker + "-env"},
			Options: map[string]string{"key": marker + "-option"},
		}},
	}
}

func mutateRichAgentConfig(agents map[string]config.AgentConfig, name, marker string) {
	agentConfig := agents[name]
	agentConfig.Role = marker
	agentConfig.Aliases[0] = marker
	agentConfig.LaneKeywords[0] = marker
	agentConfig.DetectKeywords[0] = marker
	*agentConfig.IncludeRepos = false
	agentConfig.StatsDisplay[0].Key = marker
	agentConfig.ACMMLevels[0] = 0
	*agentConfig.Channels[0].Enabled = false
	agentConfig.Channels[0].Events[0] = marker
	agentConfig.Channels[0].Patterns[0] = marker
	agentConfig.Channels[0].Match["key"] = marker
	agentConfig.Channels[0].Repos[0] = marker
	agentConfig.Tools.Preset = marker
	agentConfig.Tools.Rules[0].Pattern = marker
	agentConfig.Connections[0].Auth.Type = marker
	agentConfig.Connections[0].Options["key"] = marker
	agents[name] = agentConfig
	delete(agents, name)
}

func assertAgentSnapshot(t *testing.T, g *Governor, expected config.AgentConfig) {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	actual, exists := g.agents["quality"]
	if !exists || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("Governor agent snapshot changed through caller-owned nested state:\nactual:   %#v\nexpected: %#v", actual, expected)
	}
}

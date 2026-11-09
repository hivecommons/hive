package dashboard

import (
	"sort"

	"github.com/hivecommons/hive/pkg/config"
)

// diagAgentGitHubCaps is config_effective_read's agent_github_api_hourly_caps:
// the proxy's per-agent GitHub API hourly cap as this configuration resolves
// it. Fleet is agents.github_api_hourly_cap (or the default 300; 0 = no cap)
// and FleetSet says whether the key is written. Agents maps each declared
// agent to the cap the proxy applies to it: its own
// agents.<name>.github_api_hourly_cap, else the fleet value. Replicas and
// review heads are not listed; they take their base agent's cap.
type diagAgentGitHubCaps struct {
	Fleet     int            `json:"fleet"`
	FleetSet  bool           `json:"fleet_set"`
	Agents    map[string]int `json:"agents"`
	Overrides []string       `json:"overrides"`
}

func buildDiagAgentGitHubCaps(merged *config.Config) *diagAgentGitHubCaps {
	if merged == nil {
		return nil
	}
	// The merged layers have not been through Load's defaults; give the
	// fleet key the value an absent key gets at load, so the getter reads
	// the configuration the way the running proxy will.
	cfg := *merged
	if !cfg.AgentsGitHubAPIHourlyCapSet && cfg.AgentsGitHubAPIHourlyCap == 0 {
		cfg.AgentsGitHubAPIHourlyCap = config.DefaultAgentsGitHubAPIHourlyCap
	}
	out := &diagAgentGitHubCaps{
		Fleet:     cfg.FleetAgentGitHubAPIHourlyCap(),
		FleetSet:  cfg.AgentsGitHubAPIHourlyCapSet,
		Agents:    map[string]int{},
		Overrides: []string{},
	}
	for name, agent := range cfg.Agents {
		if agent.ReplicaOf != "" {
			continue
		}
		out.Agents[name] = cfg.AgentGitHubAPIHourlyCap(name)
		if agent.GitHubAPIHourlyCap != nil {
			out.Overrides = append(out.Overrides, name)
		}
	}
	sort.Strings(out.Overrides)
	return out
}

package dashboard

import (
	"sort"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
)

// Per-agent rows for the "GitHub API budget" panel: each agent's effective
// proxy cap (agents.<name>.github_api_hourly_cap, else its base agent's, else
// agents.github_api_hourly_cap, else 300; 0 = no cap) and the calls the proxy
// has counted against it in the current sliding hour.

var (
	agentGitHubCallsMu sync.RWMutex
	agentGitHubCallsFn func() map[string]int
)

// SetAgentGitHubCallsProvider registers the proxy's per-agent count of GitHub
// API calls in the current sliding hour (proxy.GitHubProxy.AgentGitHubCallsLastHour).
func SetAgentGitHubCallsProvider(fn func() map[string]int) {
	agentGitHubCallsMu.Lock()
	defer agentGitHubCallsMu.Unlock()
	agentGitHubCallsFn = fn
}

func getAgentGitHubCallsFn() func() map[string]int {
	agentGitHubCallsMu.RLock()
	defer agentGitHubCallsMu.RUnlock()
	return agentGitHubCallsFn
}

// buildAgentGitHubCaps is ghRateLimits.agent_caps: the fleet default and one
// row per configured agent (plus any agent the proxy counted that is no
// longer configured), sorted by name. calls is null for an uncapped agent,
// because the proxy keeps no window for it.
func buildAgentGitHubCaps(cfg *config.Config) map[string]any {
	calls := map[string]int{}
	if fn := getAgentGitHubCallsFn(); fn != nil {
		if got := fn(); got != nil {
			calls = got
		}
	}
	names := map[string]bool{}
	if cfg != nil {
		for name := range cfg.Agents {
			names[name] = true
		}
	}
	for name := range calls {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	rows := make([]map[string]any, 0, len(sorted))
	for _, name := range sorted {
		cap := cfg.AgentGitHubAPIHourlyCap(name)
		row := map[string]any{"agent": name, "cap": cap, "source": agentGitHubCapSource(cfg, name)}
		if cfg != nil {
			if a, ok := cfg.Agents[name]; ok && a.ReplicaOf != "" {
				row["base_agent"] = a.ReplicaOf
			}
		}
		if cap > 0 {
			row["calls"] = calls[name]
		} else {
			row["calls"] = nil
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"fleet_cap":     cfg.FleetAgentGitHubAPIHourlyCap(),
		"fleet_cap_set": cfg != nil && cfg.AgentsGitHubAPIHourlyCapSet,
		"window_s":      3600,
		"agents":        rows,
	}
}

// agentGitHubCapSource says where an agent's cap comes from: "agent" (its own
// override), "base_agent" (a replica or review head inheriting its base's
// override), or "fleet" (agents.github_api_hourly_cap or the default).
func agentGitHubCapSource(cfg *config.Config, name string) string {
	if cfg == nil {
		return "fleet"
	}
	a, ok := cfg.Agents[name]
	if !ok {
		return "fleet"
	}
	if a.ReplicaOf != "" {
		if base, ok := cfg.Agents[a.ReplicaOf]; ok && base.GitHubAPIHourlyCap != nil {
			return "base_agent"
		}
	}
	if a.GitHubAPIHourlyCap != nil {
		if a.ReplicaOf != "" {
			return "base_agent"
		}
		return "agent"
	}
	return "fleet"
}

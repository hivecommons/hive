package config

import (
	"sort"

	"github.com/hivecommons/hive/crux/agentmode"
)

// AgentEffectiveMode returns the agent's configured mode, preferring the
// tools-derived effective mode when one is set; a materialized replica
// (scanner-2) reads its source agent's entry. Empty when the agent is not
// configured.
func (c *Config) AgentEffectiveMode(name string) string {
	if c == nil {
		return ""
	}
	ac, ok := c.Agents[name]
	if !ok {
		ac, ok = c.Agents[c.BaseAgentName(name)]
	}
	if !ok {
		return ""
	}
	mode := ac.Mode
	if ac.Tools != nil {
		if effective := ac.Tools.EffectiveMode(); effective != "" {
			mode = effective
		}
	}
	return mode
}

// modePushesWorkflows reports whether an agent at mode gets a scoped token
// that asks for the Workflows permission: only the trusted/merger tiers do
// (agentmode.TokenTier; pkg/github ScopedTokenForRepos), so only their pushes
// may touch .github/workflows/**.
func modePushesWorkflows(mode string) bool {
	m, ok := agentmode.ParseAgentMode(mode)
	if !ok {
		return false
	}
	switch m.TokenTier() {
	case "trusted", "merger":
		return true
	}
	return false
}

// AgentPushesWorkflows reports whether agent's scoped token asks for the
// Workflows permission (trusted/merger tier), so its own push may touch
// .github/workflows/** when the installation grants it.
func (c *Config) AgentPushesWorkflows(agent string) bool {
	return c != nil && modePushesWorkflows(c.AgentEffectiveMode(agent))
}

// WorkflowsCapableLane names the enabled lane whose token can push
// .github/workflows/** and serves repo, the first by name; replicas are
// skipped because they share their source agent's lane. A lane scoped to
// other repositories is refused on repo by every enforcement point
// (AgentServesRepo), so it is never chosen there. An empty repo ignores
// scope (the fleet has such a lane somewhere). Empty when no lane can.
func (c *Config) WorkflowsCapableLane(repo string) string {
	if c == nil {
		return ""
	}
	names := make([]string, 0, len(c.Agents))
	for name, ac := range c.Agents {
		if ac.ReplicaOf != "" || !ac.Enabled || (repo != "" && !c.AgentServesRepo(name, repo)) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if modePushesWorkflows(c.AgentEffectiveMode(name)) {
			return name
		}
	}
	return ""
}

// WorkflowsLaneFor names the lane a workflow-file change in repo goes to
// when agent cannot push it: empty when agent can push it itself, or when no enabled lane
// can (then the change still needs a person). Danathar/arch-bootc#634,
// Danathar/exult#374: ISSUES_AND_PRS lanes parked approved workflow fixes for
// the owner while the hive's scanner, at ISSUES_PRS_MERGE, was landing
// workflow changes in the same repos.
func (c *Config) WorkflowsLaneFor(agent, repo string) string {
	if c == nil || modePushesWorkflows(c.AgentEffectiveMode(agent)) {
		return ""
	}
	return c.WorkflowsCapableLane(repo)
}

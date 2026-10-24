package config

import (
	"fmt"

	"github.com/hivecommons/hive/crux/skillreg"
)

// ApplyAgentSpecRepoScopes refreshes config-visible repo scopes from BYO agent
// specs. Runtime enforcement reads Config.Agents, not AgentProcess.Config, so a
// spec-owned repos: list has to be copied here during config load/reload.
//
// Operator-owned scopes win: if the dashboard or hand-edited config marks
// repos_owner: operator, that explicit operator choice is preserved instead of
// being replaced by the spec. Otherwise the spec is authoritative, including an
// omitted repos: key, which means hive-wide.
func (c *Config) ApplyAgentSpecRepoScopes() error {
	if c == nil {
		return nil
	}
	for name, agent := range c.Agents {
		if agent.AgentSpec == "" || agent.ReposOwner == FieldOwnerOperator {
			continue
		}
		spec, err := skillreg.LoadAgentSpec(agent.AgentSpec)
		if err != nil {
			return fmt.Errorf("agent %s: %w", name, err)
		}
		repos := skillreg.SpecRepos(spec)
		agentReposMu.Lock()
		agent.Repos = append([]string(nil), repos...)
		agent.ReposOwner = FieldOwnerSpec
		c.Agents[name] = agent
		agentReposMu.Unlock()
	}
	return nil
}

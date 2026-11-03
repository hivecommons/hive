package config

import (
	"fmt"
	"strings"
)

// validateContributorFirst checks the contributor-first lane keys
// (`agents.<name>.contributor_first` and `contributor_first_grace`).
//
// Like validateStandby it runs as its own pass rather than inside the agent
// loop, because the rule that matters is a relation between the agent and the
// hub block: a lane can only be covered by a role that contributors are
// allowed to act as. A contributor-first lane nobody may claim would sit
// inert forever while the operator believes contributors are taking the
// load, so — as standby does for an enabled lane with nobody approved — the
// load fails and names the fix.
func (c *Config) validateContributorFirst() error {
	for _, name := range sortedAgentNames(c.Agents) {
		agent := c.Agents[name]
		label := agentSourceLabel(name, agent.SourceFile())
		if agent.ContributorFirstGrace < 0 {
			return fmt.Errorf("agent %s: contributor_first_grace must not be negative (got %s; omit it for the default %s)",
				label, agent.ContributorFirstGrace, DefaultContributorFirstGrace)
		}
		if !agent.ContributorFirst {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(name))
		if role == "supervisor" {
			return fmt.Errorf("agent %s: contributor_first is true but the supervisor role is never delegatable to contributors", label)
		}
		if !c.Hub.IsContributeRoleDelegatable(role) {
			return fmt.Errorf("agent %s: contributor_first is true but %q is not a role contributors may act as — add it to hub.contribute_delegatable_roles (the default list is scanner, quality and outreach)",
				label, role)
		}
	}
	return nil
}

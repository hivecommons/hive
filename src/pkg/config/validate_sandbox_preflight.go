package config

import (
	"fmt"
	"sort"

	cruxconfig "github.com/hivecommons/hive/crux/config"
)

// validateSandboxPreflights checks sandbox.preflight and job.schedule_timeout_s
// in the global agent_sandbox block and in every agent's sandbox override
// (hivecommons/hive#18811), so a bad value fails config load rather than a kick.
func (c *Config) validateSandboxPreflights() error {
	if err := cruxconfig.ValidateSandboxPreflight(c.AgentSandbox.Preflight); err != nil {
		return fmt.Errorf("agent_sandbox.%w", err)
	}
	if err := cruxconfig.ValidateSandboxJobScheduleTimeout(c.AgentSandbox.Job); err != nil {
		return fmt.Errorf("agent_sandbox.%w", err)
	}
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sb := c.Agents[name].Sandbox
		if sb == nil {
			continue
		}
		if err := cruxconfig.ValidateSandboxPreflight(sb.Preflight); err != nil {
			return fmt.Errorf("agents.%s.sandbox.%w", name, err)
		}
		if err := cruxconfig.ValidateSandboxJobScheduleTimeout(sb.Job); err != nil {
			return fmt.Errorf("agents.%s.sandbox.%w", name, err)
		}
	}
	return nil
}

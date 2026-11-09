package config

// Settings-side helpers for the proxy's per-agent GitHub API hourly cap
// (agents.github_api_hourly_cap and agents.<name>.github_api_hourly_cap).
// The dashboard and the admin MCP write these through the same save path as
// every other dashboard setting; the proxy reads the live *Config through
// AgentGitHubAPIHourlyCap, so a change applies on the next proxied request.

// FleetAgentGitHubAPIHourlyCap returns the fleet default cap the proxy applies
// to an agent with no override of its own: the configured
// agents.github_api_hourly_cap, or DefaultAgentsGitHubAPIHourlyCap when the key
// is not set. 0 means no cap.
func (c *Config) FleetAgentGitHubAPIHourlyCap() int {
	if c == nil {
		return DefaultAgentsGitHubAPIHourlyCap
	}
	if !c.AgentsGitHubAPIHourlyCapSet && c.AgentsGitHubAPIHourlyCap == 0 {
		return DefaultAgentsGitHubAPIHourlyCap
	}
	return c.AgentsGitHubAPIHourlyCap
}

// SetFleetAgentGitHubAPIHourlyCap sets agents.github_api_hourly_cap. A nil cap
// removes the key, so the default applies again and the next save writes no
// key; a non-nil cap is written as given, 0 included (no cap).
func (c *Config) SetFleetAgentGitHubAPIHourlyCap(cap *int) {
	if c == nil {
		return
	}
	if cap == nil {
		// The value applyDefaults leaves for an absent key, with the "set"
		// mark cleared so the save omits the key.
		c.AgentsGitHubAPIHourlyCap = DefaultAgentsGitHubAPIHourlyCap
		c.AgentsGitHubAPIHourlyCapSet = false
		return
	}
	c.AgentsGitHubAPIHourlyCap = *cap
	c.AgentsGitHubAPIHourlyCapSet = true
}

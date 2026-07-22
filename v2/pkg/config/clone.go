package config

import (
	"maps"
	"slices"
)

// Clone returns a fully detached copy of Config. Runtime config transactions
// use it so a candidate can be validated and persisted without exposing
// partially-mutated maps, slices, or pointers to the live service.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	cloned := *c
	cloned.persistenceProvenance = slices.Clone(c.persistenceProvenance)
	for index := range cloned.persistenceProvenance {
		cloned.persistenceProvenance[index].path = slices.Clone(c.persistenceProvenance[index].path)
	}
	cloned.Project.Repos = slices.Clone(c.Project.Repos)
	if c.Project.OpenPRs != nil {
		openPRs := *c.Project.OpenPRs
		cloned.Project.OpenPRs = &openPRs
	}
	cloned.Agents = CloneAgentConfigs(c.Agents)
	cloned.Governor = CloneGovernorConfig(c.Governor)
	cloned.Variables.Security.HTTPAllowlist = slices.Clone(c.Variables.Security.HTTPAllowlist)
	if c.Variables.Defs != nil {
		cloned.Variables.Defs = make(map[string]VarDef, len(c.Variables.Defs))
		for name, definition := range c.Variables.Defs {
			clonedDefinition := definition
			if definition.Default != nil {
				defaultValue := *definition.Default
				clonedDefinition.Default = &defaultValue
			}
			clonedDefinition.Command = slices.Clone(definition.Command)
			clonedDefinition.Headers = maps.Clone(definition.Headers)
			cloned.Variables.Defs[name] = clonedDefinition
		}
	}

	if c.Notifications.Ntfy != nil {
		ntfy := *c.Notifications.Ntfy
		cloned.Notifications.Ntfy = &ntfy
	}
	if c.Notifications.Slack != nil {
		slack := *c.Notifications.Slack
		cloned.Notifications.Slack = &slack
	}
	if c.Notifications.Discord != nil {
		discord := *c.Notifications.Discord
		cloned.Notifications.Discord = &discord
	}

	cloned.Dashboard.AuthorizedUsers = slices.Clone(c.Dashboard.AuthorizedUsers)
	cloned.Knowledge.Layers = slices.Clone(c.Knowledge.Layers)
	cloned.Knowledge.Vaults = slices.Clone(c.Knowledge.Vaults)
	cloned.Knowledge.GitSources = slices.Clone(c.Knowledge.GitSources)
	cloned.Knowledge.Documents = slices.Clone(c.Knowledge.Documents)
	cloned.Knowledge.Curator.ExtractFrom = slices.Clone(c.Knowledge.Curator.ExtractFrom)
	cloned.Knowledge.Primer.Priority = slices.Clone(c.Knowledge.Primer.Priority)
	if c.Knowledge.BeadSynthesizer.Enabled != nil {
		enabled := *c.Knowledge.BeadSynthesizer.Enabled
		cloned.Knowledge.BeadSynthesizer.Enabled = &enabled
	}
	if c.Knowledge.BeadSynthesizer.RetentionPolicy != nil {
		retention := *c.Knowledge.BeadSynthesizer.RetentionPolicy
		cloned.Knowledge.BeadSynthesizer.RetentionPolicy = &retention
	}

	cloned.Hub.ContributeAllowLabels = slices.Clone(c.Hub.ContributeAllowLabels)
	cloned.Hub.ContributeDenyLabels = slices.Clone(c.Hub.ContributeDenyLabels)
	cloned.Hub.ContributeDenyTitles = slices.Clone(c.Hub.ContributeDenyTitles)
	cloned.Hub.ContributeDenyAuthors = slices.Clone(c.Hub.ContributeDenyAuthors)
	cloned.Hub.ContributeAllowModels = slices.Clone(c.Hub.ContributeAllowModels)
	cloned.Hub.DisabledRepos = slices.Clone(c.Hub.DisabledRepos)
	cloned.Hub.DisabledTiers = slices.Clone(c.Hub.DisabledTiers)
	cloned.Hub.TierLimits = maps.Clone(c.Hub.TierLimits)

	if c.ACMMLevel != nil {
		level := *c.ACMMLevel
		cloned.ACMMLevel = &level
	}
	return &cloned
}

// CloneGovernorConfig detaches every mutable collection in GovernorConfig.
func CloneGovernorConfig(governorConfig GovernorConfig) GovernorConfig {
	cloned := governorConfig
	cloned.Labels.Exempt = slices.Clone(governorConfig.Labels.Exempt)
	cloned.Sensing.GHRatePatterns = slices.Clone(governorConfig.Sensing.GHRatePatterns)
	cloned.Sensing.CLIExcludePatterns = slices.Clone(governorConfig.Sensing.CLIExcludePatterns)
	cloned.Sensing.LoginPatterns = slices.Clone(governorConfig.Sensing.LoginPatterns)
	cloned.Modes = maps.Clone(governorConfig.Modes)
	for name, mode := range cloned.Modes {
		mode.Cadences = maps.Clone(governorConfig.Modes[name].Cadences)
		cloned.Modes[name] = mode
	}
	return cloned
}

// CloneAgentConfigs detaches a complete agent registry.
func CloneAgentConfigs(agents map[string]AgentConfig) map[string]AgentConfig {
	if agents == nil {
		return nil
	}
	cloned := make(map[string]AgentConfig, len(agents))
	for name, agentConfig := range agents {
		cloned[name] = CloneAgentConfig(agentConfig)
	}
	return cloned
}

// CloneAgentConfig detaches every mutable collection and pointer in an agent
// definition, including nested channel, tool, and connection policy.
func CloneAgentConfig(agentConfig AgentConfig) AgentConfig {
	cloned := agentConfig
	cloned.Aliases = slices.Clone(agentConfig.Aliases)
	cloned.LaneKeywords = slices.Clone(agentConfig.LaneKeywords)
	cloned.DetectKeywords = slices.Clone(agentConfig.DetectKeywords)
	cloned.StatsDisplay = slices.Clone(agentConfig.StatsDisplay)
	cloned.ACMMLevels = slices.Clone(agentConfig.ACMMLevels)
	if agentConfig.IncludeRepos != nil {
		includeRepos := *agentConfig.IncludeRepos
		cloned.IncludeRepos = &includeRepos
	}

	cloned.Channels = slices.Clone(agentConfig.Channels)
	for index := range cloned.Channels {
		channel := &cloned.Channels[index]
		channel.Events = slices.Clone(agentConfig.Channels[index].Events)
		channel.Patterns = slices.Clone(agentConfig.Channels[index].Patterns)
		channel.Match = maps.Clone(agentConfig.Channels[index].Match)
		channel.Repos = slices.Clone(agentConfig.Channels[index].Repos)
		if agentConfig.Channels[index].Enabled != nil {
			enabled := *agentConfig.Channels[index].Enabled
			channel.Enabled = &enabled
		}
	}

	if agentConfig.Tools != nil {
		tools := *agentConfig.Tools
		tools.Rules = slices.Clone(agentConfig.Tools.Rules)
		cloned.Tools = &tools
	}

	cloned.Connections = slices.Clone(agentConfig.Connections)
	for index := range cloned.Connections {
		connection := &cloned.Connections[index]
		connection.Options = maps.Clone(agentConfig.Connections[index].Options)
		if agentConfig.Connections[index].Auth != nil {
			auth := *agentConfig.Connections[index].Auth
			connection.Auth = &auth
		}
	}
	return cloned
}

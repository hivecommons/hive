package config

import "strings"

// AgentProposalGateConfig identifies agent-filed direction proposals. An
// explicit block replaces the defaults, including when both lists are empty.
type AgentProposalGateConfig struct {
	Labels        []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	TitlePrefixes []string `yaml:"title_prefixes,omitempty" json:"title_prefixes,omitempty"`
}

// Equal preserves the distinction between an omitted block and an empty one.
func (g *AgentProposalGateConfig) Equal(o *AgentProposalGateConfig) bool {
	if g == nil || o == nil {
		return g == nil && o == nil
	}
	return equalStringSlices(g.Labels, o.Labels) && equalStringSlices(g.TitlePrefixes, o.TitlePrefixes)
}

// IsAgentProposal reports whether the issue matches the configured markers.
// Author and acknowledgment checks are the caller's responsibility.
func (f IssueFilterConfig) IsAgentProposal(labels []string, title string) bool {
	g := f.AgentProposal
	if g == nil {
		g = &AgentProposalGateConfig{
			Labels:        []string{"agent/strategist", "agent/outreach"},
			TitlePrefixes: []string{"[strategist]", "[outreach]"},
		}
	}
	for _, want := range g.Labels {
		if strings.TrimSpace(want) == "" {
			continue
		}
		for _, have := range labels {
			if labelMatches(want, have) {
				return true
			}
		}
	}
	title = strings.ToLower(strings.TrimSpace(title))
	for _, prefix := range g.TitlePrefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix != "" && strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return false
}

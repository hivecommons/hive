package config

import "strings"

// RepoSettings is one managed repository's per-repo knobs under the top-level
// `repos:` map (hivecommons/hive#18495). It is additive: a repo with no entry
// inherits the hive-wide value of every knob, so an absent map is exactly the
// previous behaviour.
type RepoSettings struct {
	// AgentsMdMaxBytes caps this repo's rendered AGENTS.md block, overriding
	// project.agents_md_max_bytes. Zero inherits it. The agentsmd floor and
	// hard ceiling still apply (agentsmd.EffectiveMaxBytes).
	AgentsMdMaxBytes int `yaml:"agents_md_max_bytes,omitempty" json:"agents_md_max_bytes,omitempty"`
}

// RepoSettingsFor returns repo's entry from the `repos:` map, and whether one
// exists. repo may be given bare or org-qualified, and a key may be written
// either way too: both sides are org-qualified and case-folded before they are
// compared, since GitHub repository names are case-insensitive.
func (c *Config) RepoSettingsFor(repo string) (RepoSettings, bool) {
	if c == nil || len(c.Repos) == 0 || strings.TrimSpace(repo) == "" {
		return RepoSettings{}, false
	}
	org := c.Project.Org
	want := repoPauseKey(org, strings.TrimSpace(repo))
	if rs, ok := c.Repos[repo]; ok {
		return rs, true
	}
	for key, rs := range c.Repos {
		if repoPauseKey(org, strings.TrimSpace(key)) == want {
			return rs, true
		}
	}
	return RepoSettings{}, false
}

// AgentsMdMaxBytesFor is the configured AGENTS.md injection cap for repo:
// repos.<repo>.agents_md_max_bytes when set, else
// project.agents_md_max_bytes. Zero means "use the agentsmd default"; the
// floor and ceiling are applied by the agentsmd package, not here.
func (c *Config) AgentsMdMaxBytesFor(repo string) int {
	if c == nil {
		return 0
	}
	if rs, ok := c.RepoSettingsFor(repo); ok && rs.AgentsMdMaxBytes > 0 {
		return rs.AgentsMdMaxBytes
	}
	return c.Project.AgentsMdMaxBytes
}

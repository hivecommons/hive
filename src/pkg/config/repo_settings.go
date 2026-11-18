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
	// ParentAutoClose is the decomposed-parent auto-close policy for this
	// repo (hivecommons/hive#18869): "true" (the default when empty) closes a
	// decomposed parent once every child is closed, except an issue labelled
	// epic or hold; "false" never auto-closes a parent; "epics_too" also
	// closes epic/hold-labelled parents. A parent a person reopened after a
	// hive auto-close is never re-closed under any value. YAML booleans
	// true/false are accepted and read as the strings.
	ParentAutoClose string `yaml:"parent_autoclose,omitempty" json:"parent_autoclose,omitempty"`
	// ReviewPreflight overrides review.preflight for this repo
	// (hivecommons/hive#19069). Unset fields inherit the hive-wide value.
	ReviewPreflight ReviewPreflightConfig `yaml:"review_preflight,omitempty" json:"review_preflight,omitempty"`
}

// Parent auto-close policy values for RepoSettings.ParentAutoClose.
const (
	ParentAutoCloseOn       = "true"
	ParentAutoCloseOff      = "false"
	ParentAutoCloseEpicsToo = "epics_too"
)

// ParentAutoCloseFor is the normalised parent auto-close policy for repo:
// one of ParentAutoCloseOn (default), ParentAutoCloseOff or
// ParentAutoCloseEpicsToo. An unrecognised value falls back to the default.
func (c *Config) ParentAutoCloseFor(repo string) string {
	rs, ok := c.RepoSettingsFor(repo)
	if !ok {
		return ParentAutoCloseOn
	}
	switch v := strings.ToLower(strings.TrimSpace(rs.ParentAutoClose)); v {
	case ParentAutoCloseOff, ParentAutoCloseEpicsToo:
		return v
	}
	return ParentAutoCloseOn
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

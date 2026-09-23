package config

import "time"

// ClaimsConfig governs the issue-claim ledger (hivecommons/hive#8380): the
// hub-side record of who is working an issue right now — a human session, a
// hub-kicked agent, a relay contributor ("clanker"), or an external author —
// and the rank order that decides whether a new claim takes an issue over,
// warns, or is refused. On by default; an absent block yields the defaults
// below. Set `enabled: false` to disable recording and enforcement entirely.
type ClaimsConfig struct {
	// Enabled turns the ledger on. Pointer so an omitted key defaults to
	// enabled (applyDefaults sets it), while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// HumanTTLS is how long (seconds) a human's claim lives before it lapses
	// unless renewed. 0 → default (4h).
	HumanTTLS int `yaml:"human_ttl_s" json:"human_ttl_s,omitempty"`
	// AgentTTLS is the lifetime (seconds) of a claim auto-recorded when the
	// hub kicks one of its own agents on an issue. 0 → default (2h).
	AgentTTLS int `yaml:"agent_ttl_s" json:"agent_ttl_s,omitempty"`
	// ContributorTTLS is the lifetime (seconds) of a claim auto-recorded when
	// the relay assigns an issue to an external contributor. The relay lease
	// releases it early when the task is revoked or finished. 0 → default (30m).
	ContributorTTLS int `yaml:"contributor_ttl_s" json:"contributor_ttl_s,omitempty"`
	// MaxTTLS caps any explicitly requested lifetime (seconds). 0 → default (24h).
	MaxTTLS int `yaml:"max_ttl_s" json:"max_ttl_s,omitempty"`
	// Comment posts claim / takeover / release comments on the GitHub issue.
	// Pointer so an omitted key defaults to on.
	Comment *bool `yaml:"comment" json:"comment,omitempty"`
	// Label adds a `claimed` label while an issue is held and a
	// `preempted:<login>` label when a holder is displaced. Pointer so an
	// omitted key defaults to on.
	Label *bool `yaml:"label" json:"label,omitempty"`
}

// IsEnabled reports whether the claims ledger is on. Default is ON.
func (c ClaimsConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// CommentEnabled reports whether GitHub issue comments are posted. Default ON.
func (c ClaimsConfig) CommentEnabled() bool { return c.Comment == nil || *c.Comment }

// LabelEnabled reports whether GitHub labels are applied. Default ON.
func (c ClaimsConfig) LabelEnabled() bool { return c.Label == nil || *c.Label }

// TTLs returns the configured lifetimes; zero seconds means "use the
// package default", which callers map onto claims.DefaultPolicy.
func (c ClaimsConfig) TTLs() (human, agent, contributor, max time.Duration) {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return sec(c.HumanTTLS), sec(c.AgentTTLS), sec(c.ContributorTTLS), sec(c.MaxTTLS)
}

package config

import (
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

// ClaimsConfig is `governor.claims` (hivecommons/hive#8380): whether the hive
// recognises and posts ISSUE CLAIMS — the visible, expiring "I am working on
// this" marker on an issue that covers the window before a PR exists.
//
// Off by default. With Enabled false the hive reads no claim markers, posts
// none, withholds nothing on their account and emits no claim fields on any
// listing: an absent block is zero behaviour change.
type ClaimsConfig struct {
	// Enabled turns claim recognition and agent claim posting on.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TTLS is how long a claim stands, in seconds, when the claimant set no
	// explicit expiry: the expiry the hub writes on an agent's claim, and the
	// window an assignee-inferred claim runs from the issue's last activity.
	// Absent or <= 0 → issueclaim.DefaultTTL (4h).
	TTLS int `yaml:"ttl_s,omitempty" json:"ttl_s,omitempty"`
	// HumanTTLS is how long a ranked human claim lives before it lapses unless
	// renewed. 0 falls back to TTLS, then the package default.
	HumanTTLS int `yaml:"human_ttl_s,omitempty" json:"human_ttl_s,omitempty"`
	// AgentTTLS is the lifetime of a claim auto-recorded when the hub kicks one
	// of its own agents on an issue. 0 uses the package default.
	AgentTTLS int `yaml:"agent_ttl_s,omitempty" json:"agent_ttl_s,omitempty"`
	// ContributorTTLS is the lifetime of a relay contributor claim. 0 uses the
	// package default, which matches the relay lease.
	ContributorTTLS int `yaml:"contributor_ttl_s,omitempty" json:"contributor_ttl_s,omitempty"`
	// MaxTTLS caps requested ranked-claim lifetimes. 0 uses the package default.
	MaxTTLS int `yaml:"max_ttl_s,omitempty" json:"max_ttl_s,omitempty"`
	// Comment posts claim / takeover / release comments on the GitHub issue.
	// Nil defaults to on when claims are enabled.
	Comment *bool `yaml:"comment,omitempty" json:"comment,omitempty"`
	// Label adds/removes the claimed and preempted labels. Nil defaults to on
	// when claims are enabled.
	Label *bool `yaml:"label,omitempty" json:"label,omitempty"`
}

// IsEnabled reports whether issue claims are enabled. The governor feature is
// opt-in for v5, preserving the plain issue-claim default from #8397.
func (c ClaimsConfig) IsEnabled() bool { return c.Enabled }

// EffectiveTTL returns ttl_s as a duration with the default applied.
func (c ClaimsConfig) EffectiveTTL() time.Duration {
	if c.TTLS <= 0 {
		return issueclaim.DefaultTTL
	}
	return time.Duration(c.TTLS) * time.Second
}

// CommentEnabled reports whether GitHub issue comments are posted. Default ON.
func (c ClaimsConfig) CommentEnabled() bool { return c.Comment == nil || *c.Comment }

// LabelEnabled reports whether GitHub labels are applied. Default ON.
func (c ClaimsConfig) LabelEnabled() bool { return c.Label == nil || *c.Label }

// TTLs returns ranked-claim lifetimes. Zero durations mean "use the package
// default" in pkg/claims; human_ttl_s falls back to the original ttl_s knob.
func (c ClaimsConfig) TTLs() (human, agent, contributor, max time.Duration) {
	sec := func(n int) time.Duration {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	human = sec(c.HumanTTLS)
	if human == 0 {
		human = sec(c.TTLS)
	}
	return human, sec(c.AgentTTLS), sec(c.ContributorTTLS), sec(c.MaxTTLS)
}

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
}

// EffectiveTTL returns ttl_s as a duration with the default applied.
func (c ClaimsConfig) EffectiveTTL() time.Duration {
	if c.TTLS <= 0 {
		return issueclaim.DefaultTTL
	}
	return time.Duration(c.TTLS) * time.Second
}

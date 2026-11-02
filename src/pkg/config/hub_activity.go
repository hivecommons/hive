package config

import (
	"slices"
	"strings"
)

// HubActivityConfig is the hub.activity block: the bounded, read-only
// activity digest a spoke attaches to its hub heartbeat for the hub-wide
// Activity Monitor (RFC hivecommons/hive#15861, #15963). The wire contract
// and its ceilings live in pkg/hubactivity.
type HubActivityConfig struct {
	// Enabled publishes the digest. A POINTER so an absent value means "use
	// the default": ON for a hosted spoke (hub.hive_type: hosted) whose
	// hub.heartbeat_omit does not withhold repos, OFF for every self-hosted
	// or standalone install. Resolve with HubConfig.ActivityEnabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxItems caps the items in one digest. 0/unset means 100; values above
	// the hub's finite ceiling (hubactivity.MaxItemsCeiling, 250) are clamped.
	MaxItems int `yaml:"max_items,omitempty" json:"max_items,omitempty"`
	// SharePrivate keeps items from private repositories in the digest.
	// Default false: they are dropped but still counted in totals.
	SharePrivate bool `yaml:"share_private,omitempty" json:"share_private,omitempty"`
}

// ActivityEnabled reports whether this spoke publishes its activity digest.
//
// An explicit hub.activity.enabled always wins. Otherwise the digest is on
// only for a hosted spoke, whose hub already operates the hive, and only
// when hub.heartbeat_omit does not withhold repos: a digest names
// repositories, so an operator who withheld them from the heartbeat has not
// agreed to share them here either. Self-hosted spokes stay opt-in.
func (h HubConfig) ActivityEnabled() bool {
	if h.Activity.Enabled != nil {
		return *h.Activity.Enabled
	}
	if !strings.EqualFold(strings.TrimSpace(h.HiveType), HiveTypeHosted) {
		return false
	}
	omit, err := HeartbeatOmitClasses(h.HeartbeatOmit)
	return err == nil && !slices.Contains(omit, HeartbeatOmitRepos)
}

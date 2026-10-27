package config

import (
	"time"
)

const DefaultTaskMCPLeaseRateLimitPerMinute = 60

// DefaultTaskMCPLaunchTokenTTL bounds the signed per-launch credential a
// hub-launched agent presents to the task MCP endpoint (#8348). The token is
// revoked the moment the launch it names stops being active; the TTL is the
// backstop for a launch record that outlives the hub's view of it.
const DefaultTaskMCPLaunchTokenTTL = 24 * time.Hour

type TaskMCPConfig struct {
	RemoteEnabled           bool `yaml:"remote_enabled,omitempty" json:"remote_enabled,omitempty"`
	LeaseRateLimitPerMinute int  `yaml:"lease_rate_limit_per_minute,omitempty" json:"lease_rate_limit_per_minute,omitempty"`
	// LaunchTokenTTL caps the lifetime of the per-launch MCP credential minted
	// for hub-launched agents. Zero means DefaultTaskMCPLaunchTokenTTL.
	LaunchTokenTTL time.Duration `yaml:"launch_token_ttl,omitempty" json:"launch_token_ttl,omitempty"`
}

func (c TaskMCPConfig) LeaseRateLimitPerMinuteOrDefault() int {
	if c.LeaseRateLimitPerMinute > 0 {
		return c.LeaseRateLimitPerMinute
	}
	return DefaultTaskMCPLeaseRateLimitPerMinute
}

func (c TaskMCPConfig) LaunchTokenTTLOrDefault() time.Duration {
	if c.LaunchTokenTTL > 0 {
		return c.LaunchTokenTTL
	}
	return DefaultTaskMCPLaunchTokenTTL
}

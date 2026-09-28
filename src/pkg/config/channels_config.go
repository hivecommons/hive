package config

// ChannelConfig declares a trigger channel for an agent.
//
// ChannelTypeKick has the governor runtime; ChannelTypeMention has the GitHub
// @-mention runtime, but must be paired with kick so an agent never becomes
// mention-only dormant. The former webhook/discord/schedule/bead trigger types
// were declarative-only: the pkg/channels runtime meant to serve them was never
// wired into the binary and was removed (#5591). ValidateChannels rejects them.
type ChannelConfig struct {
	Type    string `yaml:"type" json:"type"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

const (
	// ChannelTypeKick is ordinary governor timer kicks.
	ChannelTypeKick = "kick"
	// ChannelTypeMention is the GitHub @-mention trigger runtime.
	ChannelTypeMention = "mention"
)

// IsEnabled returns whether this channel is active (defaults to true).
func (c *ChannelConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ToolRule is a single allow/deny rule for a tool pattern.
type ToolRule struct {
	Pattern string `yaml:"pattern" json:"pattern"`
	Action  string `yaml:"action" json:"action"`
	Reason  string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// ToolsConfig declares what tools an agent can use.
type ToolsConfig struct {
	Preset string     `yaml:"preset,omitempty" json:"preset,omitempty"`
	Rules  []ToolRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ConnectionAuth describes how to authenticate to a connection.
type ConnectionAuth struct {
	Type   string `yaml:"type" json:"type"`
	EnvVar string `yaml:"env_var,omitempty" json:"env_var,omitempty"`
	File   string `yaml:"file,omitempty" json:"file,omitempty"`
}

// ConnectionConfig declares an external service integration for an agent.
type ConnectionConfig struct {
	Name    string            `yaml:"name" json:"name"`
	Type    string            `yaml:"type" json:"type"`
	URI     string            `yaml:"uri,omitempty" json:"uri,omitempty"`
	Auth    *ConnectionAuth   `yaml:"auth,omitempty" json:"auth,omitempty"`
	EnvName string            `yaml:"env_name,omitempty" json:"env_name,omitempty"`
	Options map[string]string `yaml:"options,omitempty" json:"options,omitempty"`
}

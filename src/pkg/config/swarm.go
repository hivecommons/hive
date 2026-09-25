package config

// SwarmConfig customizes dashboard swarm events. Empty fields preserve the
// built-in defaults and environment variables can still override them.
type SwarmConfig struct {
	EventName        string                      `yaml:"event_name,omitempty" json:"event_name,omitempty"`
	CallToArms       string                      `yaml:"call_to_arms,omitempty" json:"call_to_arms,omitempty"`
	LeaderboardTitle string                      `yaml:"leaderboard_title,omitempty" json:"leaderboard_title,omitempty"`
	Themes           map[string]SwarmThemeConfig `yaml:"themes,omitempty" json:"themes,omitempty"`
}

type SwarmThemeConfig struct {
	EventName        string `yaml:"event_name,omitempty" json:"event_name,omitempty"`
	CallToArms       string `yaml:"call_to_arms,omitempty" json:"call_to_arms,omitempty"`
	LeaderboardTitle string `yaml:"leaderboard_title,omitempty" json:"leaderboard_title,omitempty"`
}

package config

type StatsDisplayEntry struct {
	Key        string `yaml:"key" json:"key"`
	Label      string `yaml:"label" json:"label"`
	Source     string `yaml:"source" json:"source"`
	Field      string `yaml:"field" json:"field"`
	Style      string `yaml:"style" json:"style"`
	TrendField string `yaml:"trend_field,omitempty" json:"trendField,omitempty"`
	Target     int    `yaml:"target,omitempty" json:"target,omitempty"`
	// Desc is a one-line explanation of what the stat verifies, rendered
	// as a hover tooltip in the dashboard (health checks especially).
	Desc string `yaml:"desc,omitempty" json:"desc,omitempty"`
}

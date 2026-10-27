package config

// AdvisorOverride is the per-agent advisor override: every field is a pointer
// so an agent may change any part of the fleet default — or opt out entirely
// with enabled: false — while omitted fields inherit the fleet value.
type AdvisorOverride struct {
	Enabled              *bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Model                *string `yaml:"model,omitempty" json:"model,omitempty"`
	Instructions         *string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	DailyBudgetTokens    *int    `yaml:"daily_budget_tokens,omitempty" json:"daily_budget_tokens,omitempty"`
	TimeoutS             *int    `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	MaxConsecutiveBlocks *int    `yaml:"max_consecutive_blocks,omitempty" json:"max_consecutive_blocks,omitempty"`
}

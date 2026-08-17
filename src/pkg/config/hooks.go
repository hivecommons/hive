package config

// HooksConfig configures state-triggered lifecycle, governor, and sweep hooks.
type HooksConfig struct {
	// Enabled turns declarative hook dispatch on. Default false (opt-in).
	Enabled bool       `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Rules   []HookRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// IsEnabled reports whether declarative hooks are enabled.
func (h HooksConfig) IsEnabled() bool {
	return h.Enabled
}

// HookRule is a single declarative hook mapping a transition event to an action.
type HookRule struct {
	Name   string      `yaml:"name,omitempty" json:"name,omitempty"`
	On     string      `yaml:"on" json:"on"`         // e.g. on_agent_paused, on_sweep_merge, on_acmm_change
	Action string      `yaml:"action" json:"action"` // notify | script | pacing | audit
	Notify *HookNotify `yaml:"notify,omitempty" json:"notify,omitempty"`
	Script *HookScript `yaml:"script,omitempty" json:"script,omitempty"`
	Pacing *HookPacing `yaml:"pacing,omitempty" json:"pacing,omitempty"`
	Audit  *HookAudit  `yaml:"audit,omitempty" json:"audit,omitempty"`
}

// HookNotify configures a notification action when a hook fires.
type HookNotify struct {
	Title      string `yaml:"title,omitempty" json:"title,omitempty"`
	Message    string `yaml:"message,omitempty" json:"message,omitempty"`
	Channel    string `yaml:"channel,omitempty" json:"channel,omitempty"`
	WebhookURL string `yaml:"webhook_url,omitempty" json:"webhook_url,omitempty"`
}

// HookScript configures an external script / command execution action.
type HookScript struct {
	Command  string   `yaml:"command" json:"command"`
	Args     []string `yaml:"args,omitempty" json:"args,omitempty"`
	TimeoutS int      `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
}

// HookPacing configures an agent cadence / pacing adjustment action.
type HookPacing struct {
	Agent      string  `yaml:"agent,omitempty" json:"agent,omitempty"`
	Multiplier float64 `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
	Cadence    string  `yaml:"cadence,omitempty" json:"cadence,omitempty"`
}

// HookAudit configures an explicit audit log entry action.
type HookAudit struct {
	Message string `yaml:"message" json:"message"`
	Level   string `yaml:"level,omitempty" json:"level,omitempty"`
}

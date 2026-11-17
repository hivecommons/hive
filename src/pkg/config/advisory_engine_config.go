package config

import "strings"

// AdvisoryEngineConfig is phase 1 of RFC hivecommons/hive#18464: a
// report-only advisory engine that runs a CLI-less model session
// (pkg/turn/llmrouter over Model Gateways, pkg/hivetool read tools) on the
// same work items the normal lanes are kicked on, and records the result for
// comparison. It never acts on that result and performs no writes: no GitHub
// API write, no comment, label or branch, and no file outside its own
// process memory.
//
// It is DEFAULT OFF: an absent block is zero behaviour change. Enabling it
// with no mode selects shadow, the only mode phase 1 implements; any other
// mode resolves to off, so a setting this build cannot honour never selects
// a posture the operator did not ask for.
type AdvisoryEngineConfig struct {
	// Enabled turns the engine on. False is the baseline.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Mode is "shadow" (the default when enabled, and the only mode).
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Gateways names the Model Gateways (governor.gateways) the engine may
	// use, in failover order. They are references, never a second provider
	// config: endpoint, key and trains_on_inputs come from the gateway.
	Gateways []AdvisoryEngineGateway `yaml:"gateways,omitempty" json:"gateways,omitempty"`
	// AllowTrainingProvidersForPrivate is the explicit opt-in that lets a
	// gateway marked trains_on_inputs see private repository content. Off by
	// default; public repositories may use every listed gateway.
	AllowTrainingProvidersForPrivate bool `yaml:"allow_training_providers_for_private,omitempty" json:"allow_training_providers_for_private,omitempty"`
	// MaxTurns bounds one session (default 14).
	MaxTurns int `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	// ItemsPerCycle is how many new work items one eval cycle may start
	// (default 1). Sessions run one at a time in the background.
	ItemsPerCycle int `yaml:"items_per_cycle,omitempty" json:"items_per_cycle,omitempty"`
}

// AdvisoryEngineGateway references one Model Gateway.
type AdvisoryEngineGateway struct {
	// Name is a governor.gateways name.
	Name string `yaml:"name" json:"name"`
	// Model overrides the gateway's default_model.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// DailyCap is the engine's own request budget on this gateway per UTC
	// day; zero means no local cap.
	DailyCap int `yaml:"daily_cap,omitempty" json:"daily_cap,omitempty"`
}

// Advisory engine modes.
const (
	AdvisoryEngineModeOff    = "off"
	AdvisoryEngineModeShadow = "shadow"
)

// EffectiveMode resolves the mode in force: off unless enabled, shadow when
// enabled with no mode or "shadow", off for anything else.
func (a AdvisoryEngineConfig) EffectiveMode() string {
	if !a.Enabled {
		return AdvisoryEngineModeOff
	}
	switch strings.TrimSpace(a.Mode) {
	case "", AdvisoryEngineModeShadow:
		return AdvisoryEngineModeShadow
	default:
		return AdvisoryEngineModeOff
	}
}

// AdvisoryEngineMode is the nil-safe accessor for the effective mode.
func (c *Config) AdvisoryEngineMode() string {
	if c == nil {
		return AdvisoryEngineModeOff
	}
	return c.AdvisoryEngine.EffectiveMode()
}

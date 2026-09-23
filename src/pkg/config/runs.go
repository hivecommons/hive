package config

import "strings"

// ExternalRunsConfig holds one entry per external engine binding; it is the
// RunsConfig.External block (runs_config.go). The only member so far is the
// external-execution binding pilot selected by the #8201 Gate 0 decision
// (#8302) and implemented by #8361.
type ExternalRunsConfig struct {
	Flue FlueBindingConfig `yaml:"flue,omitempty" json:"flue,omitempty"`
}

// FlueBindingConfig is the operator toggle for the report-only Flue binding.
//
// It is DEFAULT OFF: an absent block is zero behaviour change. Enabling it
// with no mode selects shadow, which observes configured state but never
// starts an external run or performs an external mutation. Only an explicit
// report-only mode dispatches, and even then the engine receives no target
// repository credential, dashboard token, or publication tool.
type FlueBindingConfig struct {
	// Enabled turns the binding on. False is the pre-enrollment baseline.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Mode is "shadow" (default when enabled) or "report-only". Any other
	// value resolves to off: a setting this build cannot honour must never
	// select a posture the operator did not ask for.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Endpoint is the Flue runtime base URL. Never a credential.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// WorkflowVersion pins the engine version the runtime must advertise.
	WorkflowVersion string `yaml:"workflow_version,omitempty" json:"workflow_version,omitempty"`
}

// Flue binding modes, mirrored by pkg/extwork's Mode constants.
const (
	FlueBindingModeOff        = "off"
	FlueBindingModeShadow     = "shadow"
	FlueBindingModeReportOnly = "report-only"
	// DefaultFlueBindingMode is what an enabled binding runs in when no mode
	// is chosen.
	DefaultFlueBindingMode = FlueBindingModeShadow
)

// FlueBindingModes lists the selectable modes for settings UIs and errors.
func FlueBindingModes() []string {
	return []string{FlueBindingModeShadow, FlueBindingModeReportOnly}
}

// ValidFlueBindingMode reports whether raw is a mode an operator may select.
func ValidFlueBindingMode(raw string) bool {
	switch strings.TrimSpace(raw) {
	case FlueBindingModeShadow, FlueBindingModeReportOnly:
		return true
	default:
		return false
	}
}

// EffectiveMode resolves the mode in force: off unless enabled; shadow when
// enabled with no mode; the chosen mode when valid; off for anything else.
func (f FlueBindingConfig) EffectiveMode() string {
	if !f.Enabled {
		return FlueBindingModeOff
	}
	raw := strings.TrimSpace(f.Mode)
	if raw == "" {
		return DefaultFlueBindingMode
	}
	if ValidFlueBindingMode(raw) {
		return raw
	}
	return FlueBindingModeOff
}

// FlueBindingMode is the nil-safe accessor for the effective mode.
func (c *Config) FlueBindingMode() string {
	if c == nil {
		return FlueBindingModeOff
	}
	return c.Runs.External.Flue.EffectiveMode()
}

// FlueBindingEnabled reports whether the binding is in any mode other than off.
func (c *Config) FlueBindingEnabled() bool {
	return c.FlueBindingMode() != FlueBindingModeOff
}

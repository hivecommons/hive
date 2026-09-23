package config

import "strings"

// ExternalRunsConfig holds one entry per external engine binding; it is the
// RunsConfig.External block (runs_config.go). Flue is the batch-engine pilot
// selected by the #8201 Gate 0 decision (#8302) and implemented by #8361; OMP
// is the second, interactive host behind the same adapter (#8361 step 9,
// answering #6899). Both default off and are independent of each other.
type ExternalRunsConfig struct {
	Flue FlueBindingConfig `yaml:"flue,omitempty" json:"flue,omitempty"`
	OMP  OMPBindingConfig  `yaml:"omp,omitempty" json:"omp,omitempty"`
}

// OMPBindingConfig is the operator toggle for the report-only OMP workbench
// host. Like the Flue binding it is DEFAULT OFF, shadow when enabled with no
// mode, and dispatches only in report-only mode. There is no endpoint: the
// workbench reaches Hive over the contributor relay WebSocket it already
// holds, so the only yaml-only setting is the workflow version the workbench
// must declare. OMP is tier T3 (unconfined); the adapter refuses any
// write-capable stage, so this toggle can never widen what the host may do.
type OMPBindingConfig struct {
	// Enabled turns the host on. False is the pre-enrollment baseline.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Mode is "shadow" (default when enabled) or "report-only".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// WorkflowVersion pins the workbench workflow version a peer must declare.
	WorkflowVersion string `yaml:"workflow_version,omitempty" json:"workflow_version,omitempty"`
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

// ExternalBindingModes lists the selectable modes for settings UIs and
// errors; every external binding shares the same three postures.
func ExternalBindingModes() []string {
	return []string{FlueBindingModeShadow, FlueBindingModeReportOnly}
}

// FlueBindingModes is ExternalBindingModes under its original name.
func FlueBindingModes() []string { return ExternalBindingModes() }

// ValidExternalBindingMode reports whether raw is a mode an operator may
// select for any external binding.
func ValidExternalBindingMode(raw string) bool {
	switch strings.TrimSpace(raw) {
	case FlueBindingModeShadow, FlueBindingModeReportOnly:
		return true
	default:
		return false
	}
}

// ValidFlueBindingMode is ValidExternalBindingMode under its original name.
func ValidFlueBindingMode(raw string) bool { return ValidExternalBindingMode(raw) }

// effectiveBindingMode resolves the mode in force for an external binding:
// off unless enabled; shadow when enabled with no mode; the chosen mode when
// valid; off for anything else, so a setting this build cannot honour never
// selects a posture the operator did not ask for.
func effectiveBindingMode(enabled bool, mode string) string {
	if !enabled {
		return FlueBindingModeOff
	}
	raw := strings.TrimSpace(mode)
	if raw == "" {
		return DefaultFlueBindingMode
	}
	if ValidExternalBindingMode(raw) {
		return raw
	}
	return FlueBindingModeOff
}

// EffectiveMode resolves the mode in force for the Flue binding.
func (f FlueBindingConfig) EffectiveMode() string {
	return effectiveBindingMode(f.Enabled, f.Mode)
}

// EffectiveMode resolves the mode in force for the OMP host.
func (o OMPBindingConfig) EffectiveMode() string {
	return effectiveBindingMode(o.Enabled, o.Mode)
}

// OMPBindingMode is the nil-safe accessor for the OMP host's effective mode.
func (c *Config) OMPBindingMode() string {
	if c == nil {
		return FlueBindingModeOff
	}
	return c.Runs.External.OMP.EffectiveMode()
}

// OMPBindingEnabled reports whether the OMP host is in any mode other than off.
func (c *Config) OMPBindingEnabled() bool {
	return c.OMPBindingMode() != FlueBindingModeOff
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

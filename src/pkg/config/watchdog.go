package config

// This file holds the YAML surface of the agent self-healing watchdog
// (RFC #4665). Deliberately, NO defaults are materialized into these structs
// by applyDefaults: the #4041 login-patterns migration showed that marshaling
// defaults back into a saved config freezes them forever. The zero value of
// every field means "use the RFC default", resolved at consumption time by
// watchdog.SettingsFrom, so a hive that never mentions `governor.watchdog`
// tracks the current defaults across upgrades.

// WatchdogConfig configures the per-agent liveness/readiness reconciler.
// All duration fields accept Go duration strings ("10m", "1h30m").
type WatchdogConfig struct {
	// Enabled turns the watchdog on. Absent (nil) means enabled — the RFC
	// default — because a watchdog that ships dark heals nobody; an explicit
	// `enabled: false` opts a hive out.
	Enabled *bool `yaml:"enabled,omitempty"`
	// ProbeIntervalS is the minimum seconds between watchdog sweeps. The
	// watchdog rides the governor eval tick and self-gates to this interval,
	// so it can be slower than eval_interval_s but never faster. 0 = 300.
	ProbeIntervalS int `yaml:"probe_interval_s,omitempty"`
	// Liveness tunes the pane-classification thresholds.
	Liveness WatchdogLivenessConfig `yaml:"liveness,omitempty"`
	// Restart tunes the backoff/crash-loop reconciliation.
	Restart WatchdogRestartConfig `yaml:"restart,omitempty"`
	// Readiness tunes the production-evidence probe.
	Readiness WatchdogReadinessConfig `yaml:"readiness,omitempty"`
	// AuthProbe turns the per-provider credential probe on. Absent = on.
	AuthProbe *bool `yaml:"auth_probe,omitempty"`
}

// WatchdogLivenessConfig holds the pane state-machine thresholds.
type WatchdogLivenessConfig struct {
	// StuckOverlayAfter is how long a modal overlay (onboarding picker,
	// update prompt) may sit unanswered before the pane is classified
	// stuck-overlay and restarted. "" = 10m.
	StuckOverlayAfter string `yaml:"stuck_overlay_after,omitempty"`
	// ShellPromptAfter is how long a pane may sit at a bare shell prompt (or
	// fully silent) before it is classified dead and restarted. The grace
	// window exists because a launching CLI legitimately passes through both
	// states. "" = 5m.
	ShellPromptAfter string `yaml:"shell_prompt_after,omitempty"`
}

// WatchdogRestartConfig holds the CrashLoopBackOff-analog knobs.
type WatchdogRestartConfig struct {
	// Backoff is the per-consecutive-failure restart delay ladder; the last
	// entry is the cap. Empty = [1m, 2m, 4m, 8m, 16m].
	Backoff []string `yaml:"backoff,omitempty"`
	// CrashLoopAfter is how many consecutive failed restarts escalate to a
	// pause + alert instead of another restart. 0 = 5.
	CrashLoopAfter int `yaml:"crash_loop_after,omitempty"`
	// HealthyReset is how long an agent must stay continuously ready before
	// its failure counter resets. "" = 30m.
	HealthyReset string `yaml:"healthy_reset,omitempty"`
}

// WatchdogReadinessConfig holds the production-evidence knobs.
type WatchdogReadinessConfig struct {
	// NoProductionFor is how long an agent may show zero production evidence
	// before Producing=False is raised. "" = 6h.
	NoProductionFor string `yaml:"no_production_for,omitempty"`
}

// WatchdogEnabled resolves the tri-state Enabled field: nil means on.
func (w WatchdogConfig) WatchdogEnabled() bool {
	return w.Enabled == nil || *w.Enabled
}

// AuthProbeEnabled resolves the tri-state AuthProbe field: nil means on.
func (w WatchdogConfig) AuthProbeEnabled() bool {
	return w.AuthProbe == nil || *w.AuthProbe
}

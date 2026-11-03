package config

import "time"

// SelfExperimentSchedulerModeBackground is the only scheduler mode that runs.
const SelfExperimentSchedulerModeBackground = "background"

// DefaultSelfExperimentSchedulerInterval is the tick period when interval_s is unset.
const DefaultSelfExperimentSchedulerInterval = 5 * time.Minute

// SelfExperimentsConfig holds local settings for background Hive-self
// experiments (hivecommons/hive#12397). It is separate from the repo-scoped
// Experiment Lab and never reads or changes governor configuration.
type SelfExperimentsConfig struct {
	Scheduler SelfExperimentSchedulerConfig `yaml:"scheduler,omitempty" json:"scheduler,omitempty"`
}

// SelfExperimentSchedulerConfig gates the periodic Hive-self scheduler tick.
// Default off: it runs only when Enabled is true and Mode is exactly
// SelfExperimentSchedulerModeBackground; any other combination is a no-op.
type SelfExperimentSchedulerConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Mode must be "background" for the tick to run. Empty or any other value
	// leaves the scheduler off even when Enabled is true.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// IntervalS is the tick period in seconds. Zero or negative means
	// DefaultSelfExperimentSchedulerInterval.
	IntervalS int `yaml:"interval_s,omitempty" json:"interval_s,omitempty"`
}

// Active reports whether the background scheduler tick should run.
func (c SelfExperimentSchedulerConfig) Active() bool {
	return c.Enabled && c.Mode == SelfExperimentSchedulerModeBackground
}

// Interval returns the configured tick period or the default.
func (c SelfExperimentSchedulerConfig) Interval() time.Duration {
	if c.IntervalS <= 0 {
		return DefaultSelfExperimentSchedulerInterval
	}
	return time.Duration(c.IntervalS) * time.Second
}

package config

import "time"

// Defaults for RepeatFailureConfig (hivecommons/hive#14550, C6/C9/C12/C14).
const (
	DefaultRepeatFailureThreshold    = 5
	DefaultRepeatFailureWindowS      = 600
	DefaultRepeatFailureNoDataAfterS = 900
)

// DefaultRepeatFailureExclude returns the default waiting-loop exclusions:
// commands that fail by design while they wait (`gh pr checks` exits non-zero
// while checks are pending). An entry matches a normalized command that
// starts with it. A fresh slice is returned so callers cannot mutate the
// default.
func DefaultRepeatFailureExclude() []string {
	return []string{"gh pr checks", "gh run watch"}
}

// RepeatFailureConfig is the top-level `repeat_failure:` block: the switch for
// the monitor-only check that notices an agent repeating the same failing
// shell command (hivecommons/hive#14550).
//
// It is a POINTER on AgentRuntimeConfig so an absent block stays absent
// through a load/save cycle. Absent, or present with enabled false, means the
// check is off: Hive collects no command records and adds nothing to an
// agent's environment or home. Defaults resolve lazily through the Effective*
// accessors, which are safe on a nil receiver.
type RepeatFailureConfig struct {
	// Enabled turns the whole check on. Default false for the first release.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Threshold is how many failures of the same command with the same result
	// within the window raise a note. Absent or <= 0 means
	// DefaultRepeatFailureThreshold.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// WindowS is the sliding window, in seconds. Absent or <= 0 means
	// DefaultRepeatFailureWindowS (10 minutes).
	WindowS int `yaml:"window_s,omitempty" json:"window_s,omitempty"`
	// NoDataAfterS is how long, in seconds, an agent with shell-tool activity
	// may go without a command record before its row says the check is not
	// receiving data. Absent or <= 0 means DefaultRepeatFailureNoDataAfterS
	// (15 minutes).
	NoDataAfterS int `yaml:"no_data_after_s,omitempty" json:"no_data_after_s,omitempty"`
	// Exclude lists command prefixes that never count as repeating failures.
	// Absent or empty means DefaultRepeatFailureExclude.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
}

// IsEnabled reports whether the check is on. A nil block is off.
func (c *RepeatFailureConfig) IsEnabled() bool {
	return c != nil && c.Enabled
}

// EffectiveThreshold returns threshold with the default applied.
func (c *RepeatFailureConfig) EffectiveThreshold() int {
	if c == nil || c.Threshold <= 0 {
		return DefaultRepeatFailureThreshold
	}
	return c.Threshold
}

// EffectiveWindow returns window_s as a duration with the default applied.
func (c *RepeatFailureConfig) EffectiveWindow() time.Duration {
	if c == nil || c.WindowS <= 0 {
		return DefaultRepeatFailureWindowS * time.Second
	}
	return time.Duration(c.WindowS) * time.Second
}

// EffectiveNoDataAfter returns no_data_after_s as a duration with the default
// applied.
func (c *RepeatFailureConfig) EffectiveNoDataAfter() time.Duration {
	if c == nil || c.NoDataAfterS <= 0 {
		return DefaultRepeatFailureNoDataAfterS * time.Second
	}
	return time.Duration(c.NoDataAfterS) * time.Second
}

// EffectiveExclude returns a copy of the exclusion list, or the defaults when
// none is configured.
func (c *RepeatFailureConfig) EffectiveExclude() []string {
	if c == nil || len(c.Exclude) == 0 {
		return DefaultRepeatFailureExclude()
	}
	return append([]string(nil), c.Exclude...)
}

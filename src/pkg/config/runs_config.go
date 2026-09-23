package config

import (
	"strings"
	"time"
)

// Long-running run defaults (hivecommons/hive#8303). Every value here is a
// documented default that the `runs:` block can override; the feature itself
// is OFF until runs.spektacular.enabled is set.
const (
	// DefaultMaxStageRetries is how many times one run stage may let its lease
	// expire without reaching `document_status: final` before the runner stops
	// retrying and raises a decision escalation. The count includes the first
	// generation: at the default of 2 a stage runs once, is retried once, and
	// escalates on the second expiry, so no third generation is ever minted.
	DefaultMaxStageRetries = 2
	// DefaultRunsMaxWorktrees caps live per-stage git worktrees on one hive.
	DefaultRunsMaxWorktrees = 8
	// DefaultSpektacularBinary is the executable the runner shells out to when
	// runs.spektacular.binary is unset; it is resolved through PATH.
	DefaultSpektacularBinary = "spektacular"
	// DefaultSpektacularPollS is how often, in seconds, the runner calls the
	// status verb for each active stage lease.
	DefaultSpektacularPollS = 30
)

// RunsConfig tunes long-running runs (spec, plan, implement stages on a lease).
// The zero value keeps every existing behaviour: leases still advance only
// through the API, and nothing shells out to Spektacular.
type RunsConfig struct {
	// MaxStageRetries bounds the generations one stage may burn before the
	// runner escalates. Zero or negative means DefaultMaxStageRetries.
	MaxStageRetries int `yaml:"max_stage_retries,omitempty" json:"max_stage_retries,omitempty"`
	// MaxWorktrees caps live per-stage git worktrees. Zero or negative means
	// DefaultRunsMaxWorktrees.
	MaxWorktrees int `yaml:"max_worktrees,omitempty" json:"max_worktrees,omitempty"`
	// Spektacular configures the stage runner that polls Spektacular's status
	// verb and advances the lease on final.
	Spektacular SpektacularConfig `yaml:"spektacular,omitempty" json:"spektacular,omitempty"`
	// External holds the external-execution engine bindings: the report-only
	// Flue binding pilot and the OMP workbench host (#8361). Default off.
	External ExternalRunsConfig `yaml:"external,omitempty" json:"external,omitempty"`
}

// SpektacularConfig is the opt-in for the Spektacular stage runner.
type SpektacularConfig struct {
	// Enabled turns the poll loop on. Default false: with it off, no
	// Spektacular process is ever started by Hive.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Binary is the spektacular executable path. Empty means
	// DefaultSpektacularBinary resolved through PATH.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
	// PollIntervalS is the seconds between status calls per active stage.
	// Zero or negative means DefaultSpektacularPollS.
	PollIntervalS int `yaml:"poll_interval_s,omitempty" json:"poll_interval_s,omitempty"`
}

// MaxStageRetriesOrDefault returns the configured retry budget or the default.
func (r RunsConfig) MaxStageRetriesOrDefault() int {
	if r.MaxStageRetries <= 0 {
		return DefaultMaxStageRetries
	}
	return r.MaxStageRetries
}

func (r RunsConfig) EffectiveMaxWorktrees() int {
	if r.MaxWorktrees > 0 {
		return r.MaxWorktrees
	}
	return DefaultRunsMaxWorktrees
}

// BinaryOrDefault returns the configured executable or the default name.
func (s SpektacularConfig) BinaryOrDefault() string {
	if b := strings.TrimSpace(s.Binary); b != "" {
		return b
	}
	return DefaultSpektacularBinary
}

// PollInterval returns the configured poll cadence or the default.
func (s SpektacularConfig) PollInterval() time.Duration {
	if s.PollIntervalS <= 0 {
		return time.Duration(DefaultSpektacularPollS) * time.Second
	}
	return time.Duration(s.PollIntervalS) * time.Second
}

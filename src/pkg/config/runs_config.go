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

	// DefaultRunsWaitTimeoutSeconds is how long a run checkpoint may wait for
	// owner approval before escalation. It also floors how far a held plan
	// lease is extended so the hold does not expire out from under the owner.
	DefaultRunsWaitTimeoutSeconds = 3600
	// DefaultRunsWaitSeverity is the escalation severity for a checkpoint that
	// waited past DefaultRunsWaitTimeoutSeconds.
	DefaultRunsWaitSeverity = "decision"
	// RunImplementCheckpointMinACMM is the ACMM level a hive must reach before
	// runs.checkpoints.implement: false is honoured. Below it the implement
	// checkpoint keeps blocking regardless of config, so a young hive cannot
	// disable the last gate in front of code changes.
	RunImplementCheckpointMinACMM = 5

	// DefaultTriageMinBodyChars is the minimum useful issue body size before
	// the run triage pass asks the reporter for more detail.
	DefaultTriageMinBodyChars = 80
)

// RunsConfig tunes long-running runs (spec, plan, implement stages on a lease).
// The zero value keeps every existing behaviour: leases still advance only
// through the API, and nothing shells out to Spektacular.
type RunsConfig struct {
	// Checkpoints controls which run boundaries wait for owner approval.
	Checkpoints RunCheckpointConfig `yaml:"checkpoints,omitempty" json:"checkpoints,omitempty"`
	// WaitTimeoutSeconds is how long a checkpoint may wait before escalation.
	// Zero or negative means DefaultRunsWaitTimeoutSeconds.
	WaitTimeoutSeconds int `yaml:"wait_timeout_seconds,omitempty" json:"wait_timeout_seconds,omitempty"`
	// WaitSeverity is the escalation severity for timed-out checkpoints.
	// Empty means DefaultRunsWaitSeverity.
	WaitSeverity string `yaml:"wait_severity,omitempty" json:"wait_severity,omitempty"`
	// MaxStageRetries bounds the generations one stage may burn before the
	// runner escalates. Zero or negative means DefaultMaxStageRetries.
	MaxStageRetries int `yaml:"max_stage_retries,omitempty" json:"max_stage_retries,omitempty"`
	// MaxWorktrees caps live per-stage git worktrees. Zero or negative means
	// DefaultRunsMaxWorktrees.
	MaxWorktrees int `yaml:"max_worktrees,omitempty" json:"max_worktrees,omitempty"`
	// Triage configures the optional fix/spec/clarify decision made before
	// direct-fix dispatch. Default off.
	Triage TriageConfig `yaml:"triage,omitempty" json:"triage,omitempty"`
	// Spektacular configures the stage runner that polls Spektacular's status
	// verb and advances the lease on final.
	Spektacular SpektacularConfig `yaml:"spektacular,omitempty" json:"spektacular,omitempty"`
	// External holds the external-execution engine bindings: the report-only
	// Flue binding pilot and the OMP workbench host (#8361). Default off.
	External ExternalRunsConfig `yaml:"external,omitempty" json:"external,omitempty"`
}

// RunCheckpointConfig preserves the rollout invariant: an absent key keeps the
// historical hold-gated behavior, while explicit false lets a runner advance.
type RunCheckpointConfig struct {
	Spec      *bool `yaml:"spec,omitempty" json:"spec,omitempty"`
	Plan      *bool `yaml:"plan,omitempty" json:"plan,omitempty"`
	Implement *bool `yaml:"implement,omitempty" json:"implement,omitempty"`
}

// CheckpointBlocks reports whether the named stage boundary waits for owner
// approval. An unknown stage blocks: the safe answer for a name this build
// does not recognise is to keep the gate.
func (r RunsConfig) CheckpointBlocks(stage string) bool {
	switch strings.TrimSpace(strings.ToLower(stage)) {
	case "spec":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Spec)
	case "plan":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Plan)
	case "implement":
		return runCheckpointBoolDefaultTrue(r.Checkpoints.Implement)
	default:
		return true
	}
}

// runCheckpointBoolDefaultTrue treats an unset checkpoint key as enabled.
func runCheckpointBoolDefaultTrue(v *bool) bool {
	return v == nil || *v
}

// EffectiveWaitTimeoutSeconds returns the configured checkpoint wait budget or
// the default.
func (r RunsConfig) EffectiveWaitTimeoutSeconds() int {
	if r.WaitTimeoutSeconds > 0 {
		return r.WaitTimeoutSeconds
	}
	return DefaultRunsWaitTimeoutSeconds
}

// EffectiveWaitSeverity returns the configured escalation severity or the
// default.
func (r RunsConfig) EffectiveWaitSeverity() string {
	if s := strings.TrimSpace(strings.ToLower(r.WaitSeverity)); s != "" {
		return s
	}
	return DefaultRunsWaitSeverity
}

// TriageConfig controls the optional incoming-issue run triage pass.
type TriageConfig struct {
	// Enabled turns the pass on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// SpecLabels force a spec run when present. Empty uses the documented defaults.
	SpecLabels []string `yaml:"spec_labels,omitempty" json:"spec_labels,omitempty"`
	// FixLabels force direct-fix dispatch when present. Empty uses the documented defaults.
	FixLabels []string `yaml:"fix_labels,omitempty" json:"fix_labels,omitempty"`
	// MinBodyChars asks for clarification when the trimmed body is shorter.
	// Zero or negative means DefaultTriageMinBodyChars.
	MinBodyChars int `yaml:"min_body_chars,omitempty" json:"min_body_chars,omitempty"`
	// ClarifyComment controls posting the one-shot clarification comment. Nil defaults true.
	ClarifyComment *bool `yaml:"clarify_comment,omitempty" json:"clarify_comment,omitempty"`
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

func (t TriageConfig) EffectiveSpecLabels() []string {
	if len(t.SpecLabels) > 0 {
		return t.SpecLabels
	}
	return []string{"kind/feature", "Epic", "architecture discussion"}
}

func (t TriageConfig) EffectiveFixLabels() []string {
	if len(t.FixLabels) > 0 {
		return t.FixLabels
	}
	return []string{"kind/bug", "good first issue"}
}

func (t TriageConfig) EffectiveMinBodyChars() int {
	if t.MinBodyChars > 0 {
		return t.MinBodyChars
	}
	return DefaultTriageMinBodyChars
}

func (t TriageConfig) ShouldClarifyComment() bool {
	return t.ClarifyComment == nil || *t.ClarifyComment
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

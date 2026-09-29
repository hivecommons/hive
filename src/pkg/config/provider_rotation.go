package config

import (
	"time"
)

// ProviderBudgetConfig tunes how long the hive keeps agent kicks suspended
// after the inference provider refuses on a spending limit (#4294).
//
// There is exactly one knob because there is exactly one judgement call: how
// much wasted spend to accept in exchange for noticing sooner that the
// provider's window has reset. The hive cannot observe the reset passively —
// the suppression that saves the money also withholds the inference calls that
// would reveal the money is available again — so it periodically lets one
// cycle's kicks through as a probe.
type ProviderBudgetConfig struct {
	// ProbeIntervalS is how long a spend rebuff suppresses kicks before one
	// cycle is allowed through to test whether the provider is serving again.
	// Default 1800 (30 min).
	//
	// Shorter probes recover faster after a reset but burn a run each time on a
	// provider that is still clipped; longer probes waste less and notice later.
	// 30 minutes costs at most ~48 rebuffed runs across a day-long clip — set
	// against the field report's entire cadence firing all day. The probe is
	// how hive stays agnostic to WHEN the provider resets: reset schedules are
	// not knowable (the field report's window rolled at the key's creation time
	// of day, not at midnight — #4294), so hive never predicts one; it just
	// retries cheaply until the provider serves again.
	ProbeIntervalS int `yaml:"probe_interval_s,omitempty" json:"probe_interval_s,omitempty"`
}

// defaultProviderBudgetProbeIntervalS is the spend-rebuff probe interval when
// unset: 30 minutes.
const defaultProviderBudgetProbeIntervalS = 1800

// EffectiveProbeInterval returns the spend-rebuff probe interval, applying the
// default when unset. A negative or zero value means "unset" rather than
// "never probe": never probing is the deadlock this knob exists to prevent, so
// it is not a reachable configuration.
func (p ProviderBudgetConfig) EffectiveProbeInterval() time.Duration {
	if p.ProbeIntervalS > 0 {
		return time.Duration(p.ProbeIntervalS) * time.Second
	}
	return defaultProviderBudgetProbeIntervalS * time.Second
}

// RotationConfig configures automatic provider failover (RFC #3958). When a
// provider's subscription or credit is exhausted, agents are rotated onto a
// different provider at the same capability tier.
type RotationConfig struct {
	// Enabled turns the rotation loop on. Default false — opt-in.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ThresholdPct is the usage percentage at which a subscription provider
	// is considered exhausted (0–100). Default 85.
	ThresholdPct int `yaml:"threshold_pct,omitempty" json:"threshold_pct,omitempty"`
	// Providers maps provider name → its class config.
	Providers map[string]ProviderRotationConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	// HeadroomSource selects the provider headroom implementation.
	// "builtin" (default) keeps the in-tree probers; "ccleft" opts in to
	// github.com/tuna-os/ccleft-backed probers where available.
	HeadroomSource string `yaml:"headroom_source,omitempty" json:"headroom_source,omitempty"`
	// HighVolumeCadenceS: agents with a cadence at or below this value (seconds)
	// are high-volume and must NEVER rotate onto subscription providers.
	// Default 1800 (30 min). Protects weekly subscription budgets.
	HighVolumeCadenceS int `yaml:"high_volume_cadence_s,omitempty" json:"high_volume_cadence_s,omitempty"`
	// AgentTiers maps agent name → capability tier ("T1","T2","T3").
	// Rotation stays within tiers; drops only when nothing has headroom.
	AgentTiers map[string]string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// ProviderRotationConfig describes one provider in the rotation set.
type ProviderRotationConfig struct {
	// Class is "subscription" or "metered".
	Class string `yaml:"class" json:"class"`
	// Backends lists which hive backend names front this provider
	// (e.g. ["claude","pi"] for anthropic; ["litellm"] when litellm fronts deepseek).
	Backends []string `yaml:"backends,omitempty" json:"backends,omitempty"`
	// MonthlyAllowance states the plan's included request count per calendar
	// month for providers metered that way (github/Copilot premium requests,
	// #6980). Their documented usage endpoints report only consumption, never
	// the entitlement, so a remaining percentage exists only when the operator
	// states the allowance here. 0 (unset) leaves the reading an explicit
	// unknown rather than a guessed one.
	MonthlyAllowance int `yaml:"monthly_allowance,omitempty" json:"monthly_allowance,omitempty"`
}

// Rotation headroom source names.
const (
	RotationHeadroomSourceBuiltin = "builtin"
	RotationHeadroomSourceCCLeft  = "ccleft"
)

// defaultRotationThresholdPct is the exhaustion threshold when unset.
const defaultRotationThresholdPct = 85

// defaultHighVolumeCadenceS is the high-volume cadence cutoff when unset.
const defaultHighVolumeCadenceS = 1800

// EffectiveThreshold returns the exhaustion threshold pct, defaulting to 85.
func (r RotationConfig) EffectiveThreshold() int {
	if r.ThresholdPct > 0 {
		return r.ThresholdPct
	}
	return defaultRotationThresholdPct
}

// EffectiveHeadroomSource returns the configured headroom source, defaulting to builtin.
func (r RotationConfig) EffectiveHeadroomSource() string {
	if r.HeadroomSource == RotationHeadroomSourceCCLeft {
		return RotationHeadroomSourceCCLeft
	}
	return RotationHeadroomSourceBuiltin
}

// EffectiveHighVolumeCadenceS returns the high-volume cadence cutoff in
// seconds, defaulting to 1800 (30 minutes).
func (r RotationConfig) EffectiveHighVolumeCadenceS() int {
	if r.HighVolumeCadenceS > 0 {
		return r.HighVolumeCadenceS
	}
	return defaultHighVolumeCadenceS
}

package config

import "time"

// ForgeState defaults (hivecommons/hive#13373). Each one applies when its
// field is unset or non-positive.
const (
	DefaultForgeStateReconcileInterval = 15 * time.Minute
	DefaultForgeStateFreshnessBound    = 60 * time.Second
	DefaultForgeStateDriftThreshold    = 0.05
	DefaultForgeStateSearchBudget      = 20
)

// ForgeStateConfig is the `forgestate:` block: the per-repo GitHub state
// cache in pkg/forgestate. Default off. Nothing constructs or runs the cache
// until Enabled is true and a reader is wired to it.
type ForgeStateConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// ReconcileIntervalS is the authoritative full-list period per repo, in
	// seconds. Default 900.
	ReconcileIntervalS int `yaml:"reconcile_interval_s,omitempty" json:"reconcile_interval_s,omitempty"`
	// FreshnessBoundS is how old a cached item may be before an irreversible
	// action (merge, close, claim, kick) must re-read it. Default 60.
	FreshnessBoundS int `yaml:"freshness_bound_s,omitempty" json:"freshness_bound_s,omitempty"`
	// DriftThreshold is the fraction of items a reconcile may find wrong in
	// the cache before the repo's reconcile interval is shortened. Default 0.05.
	DriftThreshold float64 `yaml:"drift_threshold,omitempty" json:"drift_threshold,omitempty"`
	// SearchBudget caps GitHub search API calls per minute through the
	// cache's rate gate. GitHub allows 30; default 20.
	SearchBudget int `yaml:"search_budget,omitempty" json:"search_budget,omitempty"`
}

// ReconcileInterval returns the configured reconcile period or the default.
func (c ForgeStateConfig) ReconcileInterval() time.Duration {
	if c.ReconcileIntervalS <= 0 {
		return DefaultForgeStateReconcileInterval
	}
	return time.Duration(c.ReconcileIntervalS) * time.Second
}

// FreshnessBound returns the configured freshness bound or the default.
func (c ForgeStateConfig) FreshnessBound() time.Duration {
	if c.FreshnessBoundS <= 0 {
		return DefaultForgeStateFreshnessBound
	}
	return time.Duration(c.FreshnessBoundS) * time.Second
}

// DriftThresholdOrDefault returns the configured drift threshold or the default.
func (c ForgeStateConfig) DriftThresholdOrDefault() float64 {
	if c.DriftThreshold <= 0 {
		return DefaultForgeStateDriftThreshold
	}
	return c.DriftThreshold
}

// SearchBudgetPerMinute returns the configured search budget or the default.
func (c ForgeStateConfig) SearchBudgetPerMinute() int {
	if c.SearchBudget <= 0 {
		return DefaultForgeStateSearchBudget
	}
	return c.SearchBudget
}

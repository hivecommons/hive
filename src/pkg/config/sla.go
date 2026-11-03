package config

import "fmt"

// Delivery SLA targets (hivecommons/hive#11841 follow-on): the per-span
// hour budgets the dashboard Overview colours its p50s and sparklines
// against. Metrics are derived from the lifecycle timeline the hive already
// keeps; nothing here adds GitHub traffic. Zero/unset fields resolve to the
// defaults via EffectiveTargets, so an empty hive.yaml block is complete.

const (
	// DefaultSLAStartToPRHours is the default start→PR budget (hours).
	DefaultSLAStartToPRHours = 4.0
	// DefaultSLAPRToMergeHours is the default PR→merge budget (hours).
	DefaultSLAPRToMergeHours = 24.0
	// DefaultSLAOpenToMergeHours is the default open→merge budget (hours).
	DefaultSLAOpenToMergeHours = 72.0
	// MaxSLATargetHours bounds a target so a typo cannot hide every breach.
	MaxSLATargetHours = 24.0 * 365
)

// SLAConfig is the top-level `sla:` block.
type SLAConfig struct {
	Targets SLATargets `yaml:"targets,omitempty" json:"targets,omitempty"`
}

// SLATargets are per-span hour budgets. 0 = default.
type SLATargets struct {
	StartToPRH   float64 `yaml:"start_to_pr_h,omitempty" json:"start_to_pr_h,omitempty"`
	PRToMergeH   float64 `yaml:"pr_to_merge_h,omitempty" json:"pr_to_merge_h,omitempty"`
	OpenToMergeH float64 `yaml:"open_to_merge_h,omitempty" json:"open_to_merge_h,omitempty"`
}

// EffectiveTargets returns the targets with defaults filled in. Safe on nil.
func (c *SLAConfig) EffectiveTargets() SLATargets {
	t := SLATargets{}
	if c != nil {
		t = c.Targets
	}
	if t.StartToPRH <= 0 {
		t.StartToPRH = DefaultSLAStartToPRHours
	}
	if t.PRToMergeH <= 0 {
		t.PRToMergeH = DefaultSLAPRToMergeHours
	}
	if t.OpenToMergeH <= 0 {
		t.OpenToMergeH = DefaultSLAOpenToMergeHours
	}
	return t
}

// ValidateSLATargets rejects negative or absurd budgets. 0 means default.
func ValidateSLATargets(t SLATargets) error {
	for _, f := range []struct {
		name string
		v    float64
	}{{"start_to_pr_h", t.StartToPRH}, {"pr_to_merge_h", t.PRToMergeH}, {"open_to_merge_h", t.OpenToMergeH}} {
		if f.v < 0 || f.v > MaxSLATargetHours {
			return fmt.Errorf("sla.targets.%s must be between 0 (default) and %.0f hours", f.name, MaxSLATargetHours)
		}
	}
	return nil
}

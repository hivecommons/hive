package config

import (
	"strings"
)

// FormalQualityMinACMMLevel is the first maturity level at which the quality
// lane may author and maintain formal models. Below L5, quality is either
// advisory or restricted to narrower testing work, so enabling the capability
// there would grant behavior the active ACMM pack does not permit.
const FormalQualityMinACMMLevel = 5

// QualityConfig controls opt-in capabilities of the quality lane. The zero
// value preserves the existing test/coverage-only behavior.
type QualityConfig struct {
	// Formal lets the quality agent identify protocol-shaped code and add a
	// Spin/Promela model, its executable verification contract, and reporting-
	// only CI. It is deliberately opt-in and is effective only at ACMM L5+.
	Formal bool `yaml:"formal,omitempty" json:"formal,omitempty"`
}

// FormalEnabled reports whether the operator opt-in and ACMM floor both allow
// formal-model work. Keeping the floor in this effective-value method means a
// level downgrade safely disables the capability without making the persisted
// config invalid or forgetting the operator's preference.
func (q QualityConfig) FormalEnabled(acmmLevel int) bool {
	return q.Formal && acmmLevel >= FormalQualityMinACMMLevel
}

// PlanningConfig gates the Phase 4 planning entry points that fire automatically
// (as opposed to the explicit dashboard "Plan this issue" click, which is always
// available). Today that is the `plan`/`epic` label trigger: an actionable issue
// carrying one of those labels auto-mints an epic and requests decomposition.
type PlanningConfig struct {
	// PlanFromLabel enables the label trigger. Pointer so an omitted key is
	// distinguishable from an explicit false. Omitted and false are both OFF:
	// Hive never auto-detects epics. Even an explicit true is a no-op below L5,
	// because the architect that decomposes the minted epics is not scheduled
	// there.
	PlanFromLabel *bool `yaml:"plan_from_label,omitempty" json:"plan_from_label,omitempty"`
	// MirrorToIssue, when true, posts an approved plan's task checklist as a
	// comment on the epic's source GitHub issue, so people who never open the
	// dashboard can see what the plan is and how far along it is
	// (hivecommons/hive#8011). Off by default: it writes to the issue thread.
	MirrorToIssue bool `yaml:"mirror_to_issue,omitempty" json:"mirror_to_issue,omitempty"`

	// PlanLabels are the issue labels that mean "break this down" (Gate 2
	// only — same as the dashboard 📋 button). Matched case-insensitively.
	// Default DefaultPlanLabel; the previous `plan`/`epic` defaults collided
	// with repos that use `Epic` as taxonomy (RFC hivecommons/hive#7993 §7).
	PlanLabels []string `yaml:"plan_labels,omitempty" json:"plan_labels,omitempty"`
	// DesignLabels are the issue labels that mean "design first": the
	// architect posts a design on the issue, a human approves it
	// (DesignApprovedLabel), THEN it is broken down (Gate 1 → Gate 2).
	// Default DefaultDesignLabel.
	DesignLabels []string `yaml:"design_labels,omitempty" json:"design_labels,omitempty"`
	// DesignApprovedLabel is the label a human applies to approve a posted
	// design. Default DefaultDesignApprovedLabel. Applying a label needs triage
	// on the repo, which is the trust boundary the RFC settled on.
	DesignApprovedLabel string `yaml:"design_approved_label,omitempty" json:"design_approved_label,omitempty"`
	// DesignRequestedStatus is an optional source-native status/state to apply
	// alongside DesignLabels on work sources that support workflow transitions
	// (for example Jira or Linear). Empty means label-only.
	DesignRequestedStatus string `yaml:"design_requested_status,omitempty" json:"design_requested_status,omitempty"`
	// DesignApprovedStatus is an optional source-native status/state to apply
	// alongside DesignApprovedLabel on work sources that support workflow
	// transitions. Empty means label-only.
	DesignApprovedStatus string `yaml:"design_approved_status,omitempty" json:"design_approved_status,omitempty"`
	// MaxDesignRevisions caps how many times the architect is asked to revise
	// a design (a human re-applies the design label to request a revision);
	// past it the epic waits on a human. 0 = DefaultMaxDesignRevisions.
	MaxDesignRevisions int `yaml:"max_design_revisions,omitempty" json:"max_design_revisions,omitempty"`
	// MaxConcurrentDesigns caps how many designs may be in flight (posted, not
	// yet approved) at once so labeling a backlog does not starve the
	// architect's other work. 0 = DefaultMaxConcurrentDesigns.
	MaxConcurrentDesigns int `yaml:"max_concurrent_designs,omitempty" json:"max_concurrent_designs,omitempty"`
}

// Defaults for the planning label trigger (RFC hivecommons/hive#7993). The
// trigger labels are deliberately prefixed so they cannot collide with a
// repo's own `epic`/`plan` taxonomy.
const (
	DefaultPlanLabel            = "hive-plan"
	DefaultDesignLabel          = "hive-design"
	DefaultDesignApprovedLabel  = "design-approved"
	DefaultMaxDesignRevisions   = 3
	DefaultMaxConcurrentDesigns = 3
)

// PlanLabelsOrDefault returns the configured plan labels or the default set.
func (p PlanningConfig) PlanLabelsOrDefault() []string {
	if len(p.PlanLabels) > 0 {
		return p.PlanLabels
	}
	return []string{DefaultPlanLabel}
}

// DesignLabelsOrDefault returns the configured design labels or the default set.
func (p PlanningConfig) DesignLabelsOrDefault() []string {
	if len(p.DesignLabels) > 0 {
		return p.DesignLabels
	}
	return []string{DefaultDesignLabel}
}

// DesignApprovedLabelOrDefault returns the approval label or its default.
func (p PlanningConfig) DesignApprovedLabelOrDefault() string {
	if p.DesignApprovedLabel != "" {
		return p.DesignApprovedLabel
	}
	return DefaultDesignApprovedLabel
}

// MaxDesignRevisionsOrDefault returns the revision cap or its default.
func (p PlanningConfig) MaxDesignRevisionsOrDefault() int {
	if p.MaxDesignRevisions > 0 {
		return p.MaxDesignRevisions
	}
	return DefaultMaxDesignRevisions
}

// MaxConcurrentDesignsOrDefault returns the concurrency cap or its default.
func (p PlanningConfig) MaxConcurrentDesignsOrDefault() int {
	if p.MaxConcurrentDesigns > 0 {
		return p.MaxConcurrentDesigns
	}
	return DefaultMaxConcurrentDesigns
}

// RetroConfig gates the post-completion retro lane. It is off by
// default: an absent `retro:` block or `enabled: false` yields zero behavior
// change. When enabled, the lane periodically scans done/closed beads that have
// a closed/merged PR association, reconstructs a compact trajectory from local
// ledgers, and files rule-based findings as advisory beads. analysis_model is
// additionally opt-in; when empty, no model is called and only deterministic
// phase-1 behavior runs.
type RetroConfig struct {
	Enabled             bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ScanIntervalS       int    `yaml:"scan_interval_s,omitempty" json:"scan_interval_s,omitempty"`
	MaxFixAttempts      int    `yaml:"max_fix_attempts,omitempty" json:"max_fix_attempts,omitempty"`
	MaxKicks            int    `yaml:"max_kicks,omitempty" json:"max_kicks,omitempty"`
	LongStallDays       int    `yaml:"long_stall_days,omitempty" json:"long_stall_days,omitempty"`
	RecentClosedWindowS int    `yaml:"recent_closed_window_s,omitempty" json:"recent_closed_window_s,omitempty"`
	AnalysisModel       string `yaml:"analysis_model,omitempty" json:"analysis_model,omitempty"`
}

const (
	DefaultAutonomyPromoteAfter = 3
	DefaultAutonomyDemoteOn     = "rollback"
	DefaultAutonomyCooldownDays = 7
	MaxACMMLevel                = 6
	MinACMMLevel                = 1
)

type AutonomyConfig struct {
	AutoPromote  bool   `yaml:"auto_promote,omitempty" json:"auto_promote,omitempty"`
	AutoDemote   bool   `yaml:"auto_demote,omitempty" json:"auto_demote,omitempty"`
	PromoteAfter int    `yaml:"promote_after,omitempty" json:"promote_after,omitempty"`
	DemoteOn     string `yaml:"demote_on,omitempty" json:"demote_on,omitempty"`
	MaxLevel     int    `yaml:"max_level,omitempty" json:"max_level,omitempty"`
	CooldownDays int    `yaml:"cooldown_days,omitempty" json:"cooldown_days,omitempty"`
}

func (a AutonomyConfig) EffectivePromoteAfter() int {
	if a.PromoteAfter > 0 {
		return a.PromoteAfter
	}
	return DefaultAutonomyPromoteAfter
}

func (a AutonomyConfig) EffectiveDemoteOn() string {
	if strings.TrimSpace(a.DemoteOn) != "" {
		return strings.ToLower(strings.TrimSpace(a.DemoteOn))
	}
	return DefaultAutonomyDemoteOn
}

func (a AutonomyConfig) EffectiveCooldownDays() int {
	if a.CooldownDays > 0 {
		return a.CooldownDays
	}
	return DefaultAutonomyCooldownDays
}

func (a AutonomyConfig) EffectiveMaxLevel(hiveCeiling int) int {
	maxLevel := a.MaxLevel
	if maxLevel <= 0 {
		maxLevel = MaxACMMLevel
	}
	if hiveCeiling > 0 && maxLevel > hiveCeiling {
		maxLevel = hiveCeiling
	}
	if maxLevel > MaxACMMLevel {
		maxLevel = MaxACMMLevel
	}
	if maxLevel < MinACMMLevel {
		maxLevel = MinACMMLevel
	}
	return maxLevel
}

// planFromLabelMinACMM is the lowest ACMM level at which the label trigger fires
// when explicitly enabled (it is never on by default). It matches planning.PlanningMinACMMLevel: the architect agent that
// decomposes the minted epics only has a cadence at L5 (4h) and L6 (15m), so
// below L5 a minted epic would sit in decompose_pending forever. It is duplicated
// here (rather than imported) to avoid a config→planning import cycle.
const planFromLabelMinACMM = 5

// PlanFromLabelEnabled reports whether the label trigger should fire, given the
// hive's ACMM level. The trigger is OFF by default and must be explicitly
// enabled (`plan_from_label: true`): the label path hands the issue (title,
// labels and URL — the architect reads the body itself) to the architect's kick
// prompt with no per-kick review, so a maintainer merely labeling an attacker's
// issue would otherwise auto-fire attacker-controlled text into the
// highest-autonomy agent. Making it opt-in forces an operator to
// consciously accept that. When explicitly enabled, planning's own
// PlanIssuesFromLabels still applies a hard L5+ no-op (the architect that
// decomposes epics only has a cadence at L5/L6), so enabling it below L5 is
// inert — defense in depth that cannot be overridden away.
func (p PlanningConfig) PlanFromLabelEnabled(acmmLevel int) bool {
	if p.PlanFromLabel != nil {
		return *p.PlanFromLabel && acmmLevel >= planFromLabelMinACMM
	}
	return false
}

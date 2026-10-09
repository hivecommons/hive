package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Compliance defaults and bounds (hivecommons/hive#11078).
const (
	// DefaultCompliancePostureInterval is how often the posture checks run
	// when compliance.posture_checks.interval is unset.
	DefaultCompliancePostureInterval = time.Hour
	// MinCompliancePostureInterval keeps a typo ("1s") from turning the
	// posture runner into a hot loop against the GitHub API.
	MinCompliancePostureInterval = 5 * time.Minute
	// MaxCompliancePostureInterval keeps the posture history meaningful: a
	// check that runs less than weekly is not "continuous".
	MaxCompliancePostureInterval = 7 * 24 * time.Hour

	// DefaultCompliancePostureWindowDays is how far back the history-based
	// posture checks (non-author review, owner auto-merge) look when
	// compliance.posture_checks.window_days is unset (hivecommons/hive#11079).
	DefaultCompliancePostureWindowDays = 30
	// MaxCompliancePostureWindowDays bounds the GitHub search each pass runs.
	MaxCompliancePostureWindowDays = 90
	// DefaultCompliancePostureHistoryDays is how long posture-check results
	// are kept when compliance.posture_checks.history_days is unset: a year,
	// the usual SOC 2 Type II observation period.
	DefaultCompliancePostureHistoryDays = 365
	// MinCompliancePostureHistoryDays / MaxCompliancePostureHistoryDays bound
	// the history retention.
	MinCompliancePostureHistoryDays = 30
	MaxCompliancePostureHistoryDays = 3 * 365
)

// KnownComplianceFrameworks lists the framework profile IDs shipped in
// pkg/compliance/profiles. pkg/compliance imports this package, so the list
// lives here and pkg/compliance's tests assert it matches the embedded
// profiles exactly.
var KnownComplianceFrameworks = []string{"soc2-type2", "fedramp-moderate", "iso27001-annex-a"}

var complianceFrameworkIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// ComplianceConfig (top-level `compliance:`) selects the framework profiles
// whose control mapping the dashboard's Compliance surface evaluates, and how
// often the posture checks re-verify the hive against them. It never changes
// any other setting: each control points at an existing setting in its own
// section. Default off — no frameworks selected.
//
// Selecting a framework is not a certification claim; see
// docs/compliance.md.
type ComplianceConfig struct {
	// Frameworks are profile IDs, e.g. ["soc2-type2"].
	Frameworks []string `yaml:"frameworks,omitempty" json:"frameworks,omitempty"`
	// PostureChecks tunes the continuous posture-check runner.
	PostureChecks CompliancePostureChecksConfig `yaml:"posture_checks,omitempty" json:"posture_checks,omitempty"`
}

// CompliancePostureChecksConfig tunes the posture-check runner.
type CompliancePostureChecksConfig struct {
	// Interval between posture-check passes. Zero means
	// DefaultCompliancePostureInterval.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// WindowDays is the look-back of the checks that inspect merged PRs.
	// Zero means DefaultCompliancePostureWindowDays.
	WindowDays int `yaml:"window_days,omitempty" json:"window_days,omitempty"`
	// HistoryDays is how long posture-check results are retained. Zero
	// means DefaultCompliancePostureHistoryDays.
	HistoryDays int `yaml:"history_days,omitempty" json:"history_days,omitempty"`
}

// IsEnabled reports whether at least one framework is selected.
func (c ComplianceConfig) IsEnabled() bool {
	return len(c.SelectedFrameworks()) > 0
}

// SelectedFrameworks returns the configured framework IDs normalized to
// lower case, trimmed, with blanks and duplicates removed (first wins).
func (c ComplianceConfig) SelectedFrameworks() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range c.Frameworks {
		id := strings.ToLower(strings.TrimSpace(f))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// PostureIntervalOrDefault resolves the posture-check interval.
func (c ComplianceConfig) PostureIntervalOrDefault() time.Duration {
	if c.PostureChecks.Interval > 0 {
		return c.PostureChecks.Interval
	}
	return DefaultCompliancePostureInterval
}

// PostureWindowOrDefault resolves the merged-PR look-back window.
func (c ComplianceConfig) PostureWindowOrDefault() time.Duration {
	days := c.PostureChecks.WindowDays
	if days <= 0 {
		days = DefaultCompliancePostureWindowDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// PostureHistoryRetentionOrDefault resolves how long posture results are kept.
func (c ComplianceConfig) PostureHistoryRetentionOrDefault() time.Duration {
	days := c.PostureChecks.HistoryDays
	if days <= 0 {
		days = DefaultCompliancePostureHistoryDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// IsKnownComplianceFramework reports whether id names a shipped profile.
func IsKnownComplianceFramework(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, k := range KnownComplianceFrameworks {
		if k == id {
			return true
		}
	}
	return false
}

// Validate rejects malformed, unknown or duplicate framework IDs and an
// out-of-range posture interval, window or history retention, so an operator never believes a profile is
// being evaluated when a typo means nothing is.
func (c ComplianceConfig) Validate() error {
	seen := map[string]bool{}
	for i, f := range c.Frameworks {
		id := strings.ToLower(strings.TrimSpace(f))
		if id == "" {
			return fmt.Errorf("compliance.frameworks[%d]: must not be blank", i)
		}
		if !complianceFrameworkIDPattern.MatchString(id) {
			return fmt.Errorf("compliance.frameworks[%d]: %q is not a valid profile id (lower-case letters, digits and '-')", i, f)
		}
		if !IsKnownComplianceFramework(id) {
			return fmt.Errorf("compliance.frameworks[%d]: unknown framework %q (known: %s)", i, f, strings.Join(KnownComplianceFrameworks, ", "))
		}
		if seen[id] {
			return fmt.Errorf("compliance.frameworks[%d]: duplicate framework %q", i, f)
		}
		seen[id] = true
	}
	iv := c.PostureChecks.Interval
	if iv < 0 {
		return fmt.Errorf("compliance.posture_checks.interval must not be negative, got %s", iv)
	}
	if iv > 0 && iv < MinCompliancePostureInterval {
		return fmt.Errorf("compliance.posture_checks.interval must be at least %s, got %s", MinCompliancePostureInterval, iv)
	}
	if iv > MaxCompliancePostureInterval {
		return fmt.Errorf("compliance.posture_checks.interval must be at most %s, got %s", MaxCompliancePostureInterval, iv)
	}
	if w := c.PostureChecks.WindowDays; w < 0 || w > MaxCompliancePostureWindowDays {
		return fmt.Errorf("compliance.posture_checks.window_days must be between 0 and %d, got %d", MaxCompliancePostureWindowDays, w)
	}
	if h := c.PostureChecks.HistoryDays; h != 0 && (h < MinCompliancePostureHistoryDays || h > MaxCompliancePostureHistoryDays) {
		return fmt.Errorf("compliance.posture_checks.history_days must be between %d and %d, got %d", MinCompliancePostureHistoryDays, MaxCompliancePostureHistoryDays, h)
	}
	return nil
}

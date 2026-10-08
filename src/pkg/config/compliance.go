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
// out-of-range posture interval, so an operator never believes a profile is
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
	return nil
}

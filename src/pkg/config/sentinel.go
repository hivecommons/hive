package config

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/sentinel"
)

// Sentinel defaults. The label is deliberately descriptive rather than
// accusatory: a finding is a reason for a person to look, not a verdict.
const (
	DefaultSentinelLabel            = "sentinel-alert"
	DefaultSentinelLabelColor       = "b60205"
	DefaultSentinelLabelDescription = "Hive sentinel: possible security override, privilege escalation or codebase damage — a maintainer must review before merge"
	// DefaultSentinelMaxActions caps PRs labelled per sweep pass so a burst
	// of matching PRs cannot flood a repo with comments in one tick.
	DefaultSentinelMaxActions = 20
)

// SentinelConfig (top-level `sentinel:`) flags open PRs — from anyone — that
// look like attempts to override security controls, escalate privileges or
// damage the codebase (hivecommons/hive: OWNERS self-nomination, workflow
// permission widening, secret exfiltration, CI-gate softening, test deletion,
// curl|sh payloads, …). On a match the sweep ensures and applies Label, posts
// one marker-stamped comment naming the finding(s), and records an audit
// entry. The alert label is also a hard block for Hive approval and every
// auto-merge lane until a maintainer removes it.
//
// Default ON: an unset block evaluates every watched repo with
// sentinel.DefaultSensitivePaths and all behaviors enabled.
type SentinelConfig struct {
	// Enabled turns the sweep off when explicitly false. nil means on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Label is the GitHub label applied on a finding. Empty means
	// DefaultSentinelLabel.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// SensitivePaths are glob patterns (same syntax as
	// intent.guardrail_path_patterns) whose modification always raises the
	// sensitive_path finding. nil means sentinel.DefaultSensitivePaths; an
	// explicit empty list matches nothing.
	SensitivePaths []string `yaml:"sensitive_paths,omitempty" json:"sensitive_paths,omitempty"`
	// DisabledBehaviors lists sentinel rule names switched off, e.g.
	// ["test_removal"]. Unknown names are rejected on save.
	DisabledBehaviors []string `yaml:"disabled_behaviors,omitempty" json:"disabled_behaviors,omitempty"`
	// ExemptLogins are PR authors never flagged (case-insensitive exact
	// login match, e.g. "dependabot[bot]").
	ExemptLogins []string `yaml:"exempt_logins,omitempty" json:"exempt_logins,omitempty"`
	// Repos optionally restricts the sweep to these owner/repo slugs. Empty
	// means every watched repo.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// MaxActions caps PRs labelled per pass. Zero means
	// DefaultSentinelMaxActions.
	MaxActions int `yaml:"max_actions,omitempty" json:"max_actions,omitempty"`
}

// IsEnabled reports whether the sweep runs (default on).
func (s SentinelConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// LabelOrDefault resolves the alert label.
func (s SentinelConfig) LabelOrDefault() string {
	if l := strings.TrimSpace(s.Label); l != "" {
		return l
	}
	return DefaultSentinelLabel
}

// MaxActionsOrDefault resolves the per-pass action cap.
func (s SentinelConfig) MaxActionsOrDefault() int {
	if s.MaxActions > 0 {
		return s.MaxActions
	}
	return DefaultSentinelMaxActions
}

// EffectiveSensitivePaths resolves nil to the shipped defaults.
func (s SentinelConfig) EffectiveSensitivePaths() []string {
	if s.SensitivePaths == nil {
		return sentinel.DefaultSensitivePaths
	}
	return s.SensitivePaths
}

// RepoAllowed reports whether repo (owner/repo) is in scope.
func (s SentinelConfig) RepoAllowed(repo string) bool {
	if len(s.Repos) == 0 {
		return true
	}
	repo = strings.TrimSpace(repo)
	for _, r := range s.Repos {
		if strings.EqualFold(strings.TrimSpace(r), repo) {
			return true
		}
	}
	return false
}

// EvaluatorConfig converts the operator block into the pure evaluator's
// config.
func (s SentinelConfig) EvaluatorConfig() sentinel.Config {
	return sentinel.Config{
		SensitivePaths: s.SensitivePaths,
		Disabled:       s.DisabledBehaviors,
		ExemptLogins:   s.ExemptLogins,
	}
}

// ValidateSentinel rejects unknown behavior names and blank patterns so a
// typo in the dashboard cannot silently disable nothing (or everything).
func ValidateSentinel(s SentinelConfig) error {
	known := map[string]bool{}
	for _, r := range sentinel.AllRules {
		known[r] = true
	}
	for _, d := range s.DisabledBehaviors {
		if !known[strings.ToLower(strings.TrimSpace(d))] {
			return fmt.Errorf("sentinel: unknown behavior %q (known: %s)", d, strings.Join(sentinel.AllRules, ", "))
		}
	}
	for _, p := range s.SensitivePaths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("sentinel: sensitive_paths must not contain blank patterns")
		}
	}
	if s.MaxActions < 0 {
		return fmt.Errorf("sentinel: max_actions must not be negative")
	}
	return nil
}

// SentinelBehavior is one detection rule as the dashboard presents it.
type SentinelBehavior struct {
	Name        string
	Description string
}

// SentinelBehaviors lists every rule with its description, in evaluation
// order. Exposed here so the dashboard can render the Suspicious Activity
// section without importing pkg/sentinel directly (import-count ratchet).
func SentinelBehaviors() []SentinelBehavior {
	out := make([]SentinelBehavior, 0, len(sentinel.AllRules))
	for _, r := range sentinel.AllRules {
		out = append(out, SentinelBehavior{Name: r, Description: sentinel.RuleDescriptions[r]})
	}
	return out
}

// DefaultSentinelSensitivePaths returns a copy of the shipped sensitive-path
// globs.
func DefaultSentinelSensitivePaths() []string {
	return append([]string(nil), sentinel.DefaultSensitivePaths...)
}

package config

import (
	"strings"
	"sync"
)

// reviewFeedbackMu guards Config.ReviewFeedback: the dashboard's Settings card
// writes it while the review-feedback sweep reads it on the eval tick.
var reviewFeedbackMu sync.RWMutex

// ReviewFeedbackConfig is the `review_feedback:` block of hive.yaml.
type ReviewFeedbackConfig struct {
	// Rules are the preferences for capturing rules reviewers state on the
	// hive's own PRs into its knowledge (hivecommons/hive#19046,
	// hivecommons/hive#19135).
	Rules ReviewRulesSettings `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ReviewRulesSettings are the owner's preferences for reviewer-rule capture.
// Every field is optional: an unset field falls back to its environment
// variable, then to the built-in default, so a hive that never saved the
// Settings card behaves exactly as before.
type ReviewRulesSettings struct {
	// Enabled turns capture on or off (HIVE_REVIEW_FEEDBACK_RULES; default on).
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TrustedAssociations are the GitHub author associations whose rules are
	// recorded at once; every other reviewer's rule is queued for a person
	// (HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS; default OWNER, MEMBER,
	// COLLABORATOR).
	TrustedAssociations []string `yaml:"trusted_associations,omitempty" json:"trusted_associations,omitempty"`
	// ExtraPhrases are plain phrases that also mark a comment as a rule, on
	// top of the built-in signals (HIVE_REVIEW_FEEDBACK_RULE_EXTRA_PHRASES;
	// default none).
	ExtraPhrases []string `yaml:"extra_phrases,omitempty" json:"extra_phrases,omitempty"`
	// DisabledSignals are built-in signal names that no longer mark a rule
	// (HIVE_REVIEW_FEEDBACK_RULE_DISABLED_SIGNALS; default none).
	DisabledSignals []string `yaml:"disabled_signals,omitempty" json:"disabled_signals,omitempty"`
}

// ReviewAssociations are the GitHub author associations an owner may trust,
// in the order the Settings card lists them.
var ReviewAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "NONE"}

// IsReviewAssociation reports whether a is one of ReviewAssociations
// (case-insensitive).
func IsReviewAssociation(a string) bool {
	a = strings.ToUpper(strings.TrimSpace(a))
	for _, known := range ReviewAssociations {
		if a == known {
			return true
		}
	}
	return false
}

// ReviewRulesSettingsSnapshot returns a copy of the saved review-rule
// preferences (the zero value when none are saved).
func (c *Config) ReviewRulesSettingsSnapshot() ReviewRulesSettings {
	if c == nil {
		return ReviewRulesSettings{}
	}
	reviewFeedbackMu.RLock()
	defer reviewFeedbackMu.RUnlock()
	if c.ReviewFeedback == nil {
		return ReviewRulesSettings{}
	}
	return c.ReviewFeedback.Rules.clone()
}

// SetReviewRulesSettings replaces the saved review-rule preferences. Values are
// stored as given (the dashboard validates them first); an all-empty value
// removes the block, so hive.yaml only carries what the owner chose. It
// returns the previous value, for a rollback when the file cannot be written.
func (c *Config) SetReviewRulesSettings(s ReviewRulesSettings) ReviewRulesSettings {
	reviewFeedbackMu.Lock()
	defer reviewFeedbackMu.Unlock()
	var prev ReviewRulesSettings
	if c.ReviewFeedback != nil {
		prev = c.ReviewFeedback.Rules.clone()
	}
	s = s.clone()
	if s.Enabled == nil && len(s.TrustedAssociations) == 0 && len(s.ExtraPhrases) == 0 && len(s.DisabledSignals) == 0 {
		c.ReviewFeedback = nil
		return prev
	}
	c.ReviewFeedback = &ReviewFeedbackConfig{Rules: s}
	return prev
}

func (s ReviewRulesSettings) clone() ReviewRulesSettings {
	out := ReviewRulesSettings{
		TrustedAssociations: append([]string(nil), s.TrustedAssociations...),
		ExtraPhrases:        append([]string(nil), s.ExtraPhrases...),
		DisabledSignals:     append([]string(nil), s.DisabledSignals...),
	}
	if s.Enabled != nil {
		v := *s.Enabled
		out.Enabled = &v
	}
	return out
}

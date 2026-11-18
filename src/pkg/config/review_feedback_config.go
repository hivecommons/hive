package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/reviewrules"
)

// Review feedback rules (hivecommons/hive#19138): the preferences of the
// capture that records rules reviewers state on the hive's own PRs
// (src/docs/review-feedback-rules.md). Every key is optional and additive.
// The existing env vars (HIVE_REVIEW_FEEDBACK_RULES,
// HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS) still win when set, so a
// deployment that already uses them keeps its behavior. The sweep reads these
// keys on every pass, so an edit applies without a restart.

// ReviewFeedbackAssociations are the GitHub author associations
// review_feedback.rules.trusted_associations accepts.
var ReviewFeedbackAssociations = []string{
	"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR",
	"FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE",
}

// ReviewFeedbackConfig is the review_feedback block of hive.yaml.
type ReviewFeedbackConfig struct {
	Rules ReviewFeedbackRulesConfig `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ReviewFeedbackRulesConfig is review_feedback.rules.
type ReviewFeedbackRulesConfig struct {
	// Enabled turns rule capture on or off. Unset means on. The review
	// feedback sweep keeps running either way.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TrustedAssociations are the author associations whose rules are
	// recorded at once; every other reviewer's rule is queued for a person.
	// Empty means OWNER, MEMBER, COLLABORATOR.
	TrustedAssociations []string `yaml:"trusted_associations,omitempty" json:"trusted_associations,omitempty"`
	// ExtraPhrases are literal phrases that also mark a comment as a rule,
	// matched case-insensitively on word boundaries.
	ExtraPhrases []string `yaml:"extra_phrases,omitempty" json:"extra_phrases,omitempty"`
	// DisabledSignals are built-in signal names the classifier skips, for
	// example "authoritative" on a project where the word is common.
	DisabledSignals []string `yaml:"disabled_signals,omitempty" json:"disabled_signals,omitempty"`
}

// Settings converts the block to the capture's own settings type.
func (r ReviewFeedbackRulesConfig) Settings() reviewrules.Settings {
	return reviewrules.Settings{
		Enabled:             r.Enabled,
		TrustedAssociations: r.TrustedAssociations,
		ExtraPhrases:        r.ExtraPhrases,
		DisabledSignals:     r.DisabledSignals,
	}
}

// Validate rejects unknown associations and signals and unusable phrases.
func (r ReviewFeedbackRulesConfig) Validate() error {
	for _, a := range r.TrustedAssociations {
		if !slices.Contains(ReviewFeedbackAssociations, strings.ToUpper(strings.TrimSpace(a))) {
			return fmt.Errorf("review_feedback.rules.trusted_associations: %q is not one of %s", a, strings.Join(ReviewFeedbackAssociations, ", "))
		}
	}
	if len(r.ExtraPhrases) > reviewrules.MaxExtraPhrases {
		return fmt.Errorf("review_feedback.rules.extra_phrases: at most %d phrases, got %d", reviewrules.MaxExtraPhrases, len(r.ExtraPhrases))
	}
	for i, p := range r.ExtraPhrases {
		if err := reviewrules.ValidExtraPhrase(p); err != nil {
			return fmt.Errorf("review_feedback.rules.extra_phrases[%d] %s", i, err)
		}
	}
	for _, s := range r.DisabledSignals {
		if !reviewrules.IsSignal(strings.ToLower(strings.TrimSpace(s))) {
			return fmt.Errorf("review_feedback.rules.disabled_signals: %q is not one of %s", s, strings.Join(reviewrules.SignalNames(), ", "))
		}
	}
	return nil
}

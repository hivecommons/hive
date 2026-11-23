package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stranded review-feedback retry knobs (hivecommons/hive#19320). They live
// here, not in pkg/reviewfeedback, so the dashboard's Settings card can name
// them without importing the sweep.
const (
	// ReviewRetryMaxEnvVar overrides the retry cap (0 asks a person at once).
	ReviewRetryMaxEnvVar = "HIVE_REVIEW_FEEDBACK_RETRY_MAX"
	// ReviewRetryDefaultMax is how many times stranded feedback is re-routed.
	ReviewRetryDefaultMax = 2
	// ReviewRetryBackoffEnvVar overrides the base backoff.
	ReviewRetryBackoffEnvVar = "HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF"
	// ReviewRetryDefaultBackoff is the wait after a kick before it is judged
	// stranded; it doubles on each retry.
	ReviewRetryDefaultBackoff = time.Hour
	// ReviewRetryMinBackoff keeps a misconfigured backoff from re-kicking an
	// agent still working on the first brief.
	ReviewRetryMinBackoff = 10 * time.Minute
	// ReviewRetryMaxMax bounds the cap an owner may set.
	ReviewRetryMaxMax = 10
)

// ReviewRetryFromEnv returns the retry cap and backoff from the environment,
// falling back to the defaults for an unset or invalid value. The caller
// clamps them.
func ReviewRetryFromEnv() (int, time.Duration) {
	maxRetries, backoff := ReviewRetryDefaultMax, ReviewRetryDefaultBackoff
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(ReviewRetryMaxEnvVar))); err == nil && n >= 0 {
		maxRetries = n
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(ReviewRetryBackoffEnvVar))); err == nil && d > 0 {
		backoff = d
	}
	return maxRetries, backoff
}

// reviewFeedbackMu guards Config.ReviewFeedback: the dashboard's Settings card
// writes it while the review-feedback sweep reads it on the eval tick.
var reviewFeedbackMu sync.RWMutex

// ReviewFeedbackConfig is the `review_feedback:` block of hive.yaml.
type ReviewFeedbackConfig struct {
	// Rules are the preferences for capturing rules reviewers state on the
	// hive's own PRs into its knowledge (hivecommons/hive#19046,
	// hivecommons/hive#19135).
	Rules ReviewRulesSettings `yaml:"rules,omitempty" json:"rules,omitempty"`
	// Retry is the owner's preferences for re-routing feedback a kicked agent
	// left stranded: no push and no reply (hivecommons/hive#19320).
	Retry ReviewRetrySettings `yaml:"retry,omitempty" json:"retry,omitempty"`
}

// ReviewRetrySettings are the owner's preferences for re-routing stranded
// review feedback. Every field is optional: an unset field falls back to its
// environment variable, then to the built-in default.
type ReviewRetrySettings struct {
	// MaxRetries is how many times stranded feedback is routed again before
	// a person is asked (HIVE_REVIEW_FEEDBACK_RETRY_MAX; default 2). 0 turns
	// re-routing off: stranded feedback is handed to a person at once.
	MaxRetries *int `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
	// Backoff is how long after a kick the sweep waits before judging it
	// stranded; it doubles on each retry (HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF;
	// default 1h). A Go duration such as "45m" or "2h".
	Backoff string `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

func (s ReviewRetrySettings) empty() bool { return s.MaxRetries == nil && s.Backoff == "" }

func (s ReviewRetrySettings) clone() ReviewRetrySettings {
	out := ReviewRetrySettings{Backoff: s.Backoff}
	if s.MaxRetries != nil {
		v := *s.MaxRetries
		out.MaxRetries = &v
	}
	return out
}

func (s ReviewRulesSettings) empty() bool {
	return s.Enabled == nil && len(s.TrustedAssociations) == 0 && len(s.ExtraPhrases) == 0 && len(s.DisabledSignals) == 0
}

// ReviewRetrySettingsSnapshot returns a copy of the saved stranded-feedback
// retry preferences (the zero value when none are saved).
func (c *Config) ReviewRetrySettingsSnapshot() ReviewRetrySettings {
	if c == nil {
		return ReviewRetrySettings{}
	}
	reviewFeedbackMu.RLock()
	defer reviewFeedbackMu.RUnlock()
	if c.ReviewFeedback == nil {
		return ReviewRetrySettings{}
	}
	return c.ReviewFeedback.Retry.clone()
}

// SetReviewRetrySettings replaces the saved retry preferences, keeping the
// rule preferences, and returns the previous value for a rollback. An empty
// block is dropped so hive.yaml only carries what the owner chose.
func (c *Config) SetReviewRetrySettings(s ReviewRetrySettings) ReviewRetrySettings {
	reviewFeedbackMu.Lock()
	defer reviewFeedbackMu.Unlock()
	var prev ReviewRetrySettings
	var rules ReviewRulesSettings
	if c.ReviewFeedback != nil {
		prev = c.ReviewFeedback.Retry.clone()
		rules = c.ReviewFeedback.Rules
	}
	c.ReviewFeedback = packReviewFeedback(rules, s.clone())
	return prev
}

// packReviewFeedback builds the block, nil when both halves are empty.
func packReviewFeedback(rules ReviewRulesSettings, retry ReviewRetrySettings) *ReviewFeedbackConfig {
	if rules.empty() && retry.empty() {
		return nil
	}
	return &ReviewFeedbackConfig{Rules: rules, Retry: retry}
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
// drops the rules half (and the block when retry is unset too), so hive.yaml
// only carries what the owner chose; saved retry preferences are kept. It
// returns the previous value, for a rollback when the file cannot be written.
func (c *Config) SetReviewRulesSettings(s ReviewRulesSettings) ReviewRulesSettings {
	reviewFeedbackMu.Lock()
	defer reviewFeedbackMu.Unlock()
	var prev ReviewRulesSettings
	var retry ReviewRetrySettings
	if c.ReviewFeedback != nil {
		prev = c.ReviewFeedback.Rules.clone()
		retry = c.ReviewFeedback.Retry
	}
	c.ReviewFeedback = packReviewFeedback(s.clone(), retry)
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

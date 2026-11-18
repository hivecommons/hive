package config

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ReviewFeedbackConfig is the `review_feedback:` block of hive.yaml
// (hivecommons/hive#19138). Its `rules` block holds the preferences for
// capturing reviewer-stated rules into the hive's knowledge
// (src/docs/review-feedback-rules.md). Every key is additive: leave the block
// out and the capture behaves exactly as it did, and the
// HIVE_REVIEW_FEEDBACK_RULES and HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS
// environment variables keep working.
type ReviewFeedbackConfig struct {
	Rules ReviewFeedbackRulesConfig `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// ReviewFeedbackRulesConfig is `review_feedback.rules`.
type ReviewFeedbackRulesConfig struct {
	// Enabled turns reviewer-stated rule capture on or off. Nil keeps the
	// default (on). The review-feedback sweep itself is not affected.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TrustedAssociations are the GitHub author associations whose rules are
	// captured at once; every other reviewer's rule is queued for a person.
	// Empty keeps OWNER, MEMBER and COLLABORATOR.
	TrustedAssociations []string `yaml:"trusted_associations,omitempty" json:"trusted_associations,omitempty"`
	// ExtraPhrases are additional phrases that mark a comment as a rule.
	// Matching is case-insensitive and on word boundaries.
	ExtraPhrases []string `yaml:"extra_phrases,omitempty" json:"extra_phrases,omitempty"`
	// DisabledSignals names built-in signals that no longer mark a comment as
	// a rule (for example "knowledge-base").
	DisabledSignals []string `yaml:"disabled_signals,omitempty" json:"disabled_signals,omitempty"`
}

// ReviewFeedbackAssociations are the GitHub author associations a rule's
// reviewer can have.
var ReviewFeedbackAssociations = []string{
	"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE",
}

// ReviewFeedbackSignals are the built-in signal names disabled_signals may
// name. It mirrors the Signal* constants in pkg/reviewrules, which a test keeps
// in step (pkg/config cannot import it).
var ReviewFeedbackSignals = []string{
	"source-of-truth", "authoritative", "always", "never", "going-forward", "capture-request", "assessment-criteria", "knowledge-base",
}

const (
	// MaxReviewFeedbackExtraPhrases bounds extra_phrases.
	MaxReviewFeedbackExtraPhrases = 50
	// MaxReviewFeedbackPhraseRunes bounds one extra phrase.
	MaxReviewFeedbackPhraseRunes = 100
	// MinReviewFeedbackPhraseRunes keeps a phrase specific enough not to match
	// ordinary prose.
	MinReviewFeedbackPhraseRunes = 4
)

var reviewFeedbackMu sync.RWMutex

// ReviewFeedbackRules returns a copy of review_feedback.rules, safe to read
// while the dashboard edits it.
func (c *Config) ReviewFeedbackRules() ReviewFeedbackRulesConfig {
	if c == nil {
		return ReviewFeedbackRulesConfig{}
	}
	reviewFeedbackMu.RLock()
	defer reviewFeedbackMu.RUnlock()
	r := c.ReviewFeedback.Rules
	if r.Enabled != nil {
		v := *r.Enabled
		r.Enabled = &v
	}
	r.TrustedAssociations = append([]string(nil), r.TrustedAssociations...)
	r.ExtraPhrases = append([]string(nil), r.ExtraPhrases...)
	r.DisabledSignals = append([]string(nil), r.DisabledSignals...)
	return r
}

// SetReviewFeedbackRules replaces review_feedback.rules with the normalised,
// validated r. Nothing changes on an error.
func (c *Config) SetReviewFeedbackRules(r ReviewFeedbackRulesConfig) error {
	if c == nil {
		return fmt.Errorf("config unavailable")
	}
	norm, err := NormalizeReviewFeedbackRules(r)
	if err != nil {
		return err
	}
	reviewFeedbackMu.Lock()
	defer reviewFeedbackMu.Unlock()
	c.ReviewFeedback.Rules = norm
	return nil
}

// NormalizeReviewFeedbackRules validates r and returns it in canonical form:
// associations and signals upper/lower-cased, sorted and de-duplicated, phrases
// trimmed with inner whitespace collapsed and de-duplicated case-insensitively.
func NormalizeReviewFeedbackRules(r ReviewFeedbackRulesConfig) (ReviewFeedbackRulesConfig, error) {
	out := ReviewFeedbackRulesConfig{}
	if r.Enabled != nil {
		v := *r.Enabled
		out.Enabled = &v
	}
	assocOK := map[string]bool{}
	for _, a := range ReviewFeedbackAssociations {
		assocOK[a] = true
	}
	seen := map[string]bool{}
	for _, a := range r.TrustedAssociations {
		a = strings.ToUpper(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		if !assocOK[a] {
			return ReviewFeedbackRulesConfig{}, fmt.Errorf("review_feedback.rules.trusted_associations: %q is not a GitHub author association (one of %s)", a, strings.Join(ReviewFeedbackAssociations, ", "))
		}
		seen[a] = true
		out.TrustedAssociations = append(out.TrustedAssociations, a)
	}
	sort.Strings(out.TrustedAssociations)

	sigOK := map[string]bool{}
	for _, s := range ReviewFeedbackSignals {
		sigOK[s] = true
	}
	seen = map[string]bool{}
	for _, s := range r.DisabledSignals {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		if !sigOK[s] {
			return ReviewFeedbackRulesConfig{}, fmt.Errorf("review_feedback.rules.disabled_signals: %q is not a built-in signal (one of %s)", s, strings.Join(ReviewFeedbackSignals, ", "))
		}
		seen[s] = true
		out.DisabledSignals = append(out.DisabledSignals, s)
	}
	sort.Strings(out.DisabledSignals)

	seen = map[string]bool{}
	for _, p := range r.ExtraPhrases {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		n := len([]rune(p))
		if n < MinReviewFeedbackPhraseRunes || n > MaxReviewFeedbackPhraseRunes {
			return ReviewFeedbackRulesConfig{}, fmt.Errorf("review_feedback.rules.extra_phrases: %q must be %d to %d characters", p, MinReviewFeedbackPhraseRunes, MaxReviewFeedbackPhraseRunes)
		}
		seen[strings.ToLower(p)] = true
		out.ExtraPhrases = append(out.ExtraPhrases, p)
		if len(out.ExtraPhrases) > MaxReviewFeedbackExtraPhrases {
			return ReviewFeedbackRulesConfig{}, fmt.Errorf("review_feedback.rules.extra_phrases: at most %d phrases", MaxReviewFeedbackExtraPhrases)
		}
	}
	return out, nil
}

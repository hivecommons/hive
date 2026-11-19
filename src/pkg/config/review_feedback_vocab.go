package config

import "github.com/hivecommons/hive/pkg/reviewrules"

// The review-rule vocabulary the Settings card needs, re-exported from the
// stdlib-only pkg/reviewrules leaf so the dashboard validates against the same
// table the sweep classifies with without importing another package
// (hivecommons/hive#19135).

// ReviewRuleSignal is one built-in rule signal and its examples.
type ReviewRuleSignal = reviewrules.SignalInfo

// Env vars and limits behind review_feedback.rules.*.
const (
	ReviewRulesEnabledEnvVar         = reviewrules.DisableEnvVar
	ReviewRulesTrustedEnvVar         = reviewrules.TrustedAssociationsEnvVar
	ReviewRulesExtraPhrasesEnvVar    = reviewrules.ExtraPhrasesEnvVar
	ReviewRulesDisabledSignalsEnvVar = reviewrules.DisabledSignalsEnvVar
	ReviewRulesDefaultTrusted        = reviewrules.DefaultTrustedAssociations
	ReviewRulesMaxPhrases            = reviewrules.MaxPhrases
	ReviewRulesMaxPhraseRunes        = reviewrules.MaxPhraseRunes
)

// ReviewRuleSignals lists the built-in signals in table order.
func ReviewRuleSignals() []ReviewRuleSignal { return reviewrules.Signals() }

// KnownReviewRuleSignal reports whether name is a built-in signal.
func KnownReviewRuleSignal(name string) bool { return reviewrules.KnownSignal(name) }

// ValidateReviewRulePhrase says why an extra phrase is unusable, or "".
func ValidateReviewRulePhrase(p string) string { return reviewrules.ValidatePhrase(p) }

// ReviewRulesEnvEnabled reports whether HIVE_REVIEW_FEEDBACK_RULES leaves
// capture on (the default).
func ReviewRulesEnvEnabled() bool { return reviewrules.Enabled() }

// ParseReviewRulesList splits a comma-separated list into trimmed items.
func ParseReviewRulesList(list string) []string { return reviewrules.ParseList(list) }
